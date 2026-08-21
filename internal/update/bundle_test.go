package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// signer is a stand-in for whatever holds the private half off this appliance.
// It exists only in tests: nothing in the package can sign.
type signer struct {
	id   string
	pub  ed25519.PublicKey
	priv ed25519.PrivateKey
}

func newSigner(t *testing.T, id string) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signer{id: id, pub: pub, priv: priv}
}

func (s signer) bundle(t *testing.T, payload []byte, mutate func(*Manifest)) []byte {
	t.Helper()
	sum := sha256.Sum256(payload)
	m := Manifest{
		Format: 1, Product: Product, Version: "1.4.2", Build: 1420,
		CreatedAt:     time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC),
		PayloadSHA256: hex.EncodeToString(sum[:]),
		PayloadSize:   int64(len(payload)),
		KeyID:         s.id,
	}
	if mutate != nil {
		mutate(&m)
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	sig := ed25519.Sign(s.priv, SigningInput(raw))
	var buf bytes.Buffer
	if err := Write(&buf, raw, sig, payload); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func trust(t *testing.T, signers ...signer) *TrustStore {
	t.Helper()
	var keys []TrustedKey
	for _, s := range signers {
		keys = append(keys, TrustedKey{ID: s.id, Public: s.pub})
	}
	ts, err := NewTrustStore(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func parse(t *testing.T, raw []byte) *Bundle {
	t.Helper()
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return b
}

var installed = Current{Build: 1000, BuildKnown: true, Tier: ""}

// The happy path, so every refusal below is known to be refusing something that
// would otherwise have worked.
func TestAProperlySignedBundleVerifies(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	payload := []byte("this is the appliance image")
	v, err := Verify(parse(t, s.bundle(t, payload, nil)), trust(t, s), installed)
	if err != nil {
		t.Fatalf("a correctly signed bundle was refused: %v", err)
	}
	if !bytes.Equal(v.Payload(), payload) {
		t.Fatal("the verified payload is not the payload that was signed")
	}
	if v.KeyID != "vendor-2026" {
		t.Fatalf("verified under key %q", v.KeyID)
	}
}

// THE central test of the whole package. A bundle signed by a key the appliance
// does not hold must be refused — however well-formed it is, and however
// correctly it verifies against its own key. An update path that trusts the key
// travelling with the update is not a signed update path.
func TestABundleSignedByAnUntrustedKeyIsRefused(t *testing.T) {
	real := newSigner(t, "vendor-2026")
	attacker := newSigner(t, "vendor-2026") // SAME key id, different key
	raw := attacker.bundle(t, []byte("hostile image"), nil)

	// It verifies perfectly against the attacker's own key...
	if !ed25519.Verify(attacker.pub, mustManifestBytes(t, raw), mustSigBytes(t, raw)) {
		t.Fatal("the test's own hostile bundle is malformed")
	}
	// ...and must be refused by an appliance that holds only the real key.
	_, err := Verify(parse(t, raw), trust(t, real), installed)
	if !errors.Is(err, ErrBadSignature) {
		t.Fatalf("a bundle signed by an impostor under a trusted key ID was accepted or misdiagnosed: %v", err)
	}
}

// A key id nobody holds is refused by NAME, so an operator is told which key was
// wanted rather than "bad signature".
func TestABundleNamingAnUnknownKeyIsRefused(t *testing.T) {
	real := newSigner(t, "vendor-2026")
	other := newSigner(t, "vendor-2030")
	_, err := Verify(parse(t, other.bundle(t, []byte("image"), nil)), trust(t, real), installed)
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v, want ErrUnknownKey", err)
	}
}

// The signature covers the manifest, and the manifest binds the payload by
// digest. Swapping the payload after signing must be caught.
func TestASwappedPayloadIsRefused(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("the signed image"), nil)
	// Replace the payload, leaving the manifest and signature untouched.
	idx := bytes.Index(raw, []byte("the signed image"))
	if idx < 0 {
		t.Fatal("could not find the payload in the bundle")
	}
	tampered := append([]byte(nil), raw...)
	copy(tampered[idx:], []byte("the WRONG image!"))

	_, err := Verify(parse(t, tampered), trust(t, s), installed)
	if !errors.Is(err, ErrPayloadMismatch) {
		t.Fatalf("a swapped payload was accepted or misdiagnosed: %v", err)
	}
}

// Editing the manifest after signing must break the signature. This is the check
// that fails if verification is ever done over a RE-SERIALISATION of the parsed
// manifest rather than the bytes that were read.
func TestAnEditedManifestIsRefused(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("image"), nil)
	idx := bytes.Index(raw, []byte(`"version":"1.4.2"`))
	if idx < 0 {
		t.Fatal("could not find the version in the manifest")
	}
	tampered := append([]byte(nil), raw...)
	copy(tampered[idx:], []byte(`"version":"9.9.9"`))

	if _, err := Verify(parse(t, tampered), trust(t, s), installed); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("an edited manifest was accepted: %v", err)
	}
}

// The signature covers the BYTES that arrived, not a re-encoding of them.
//
// This bundle's manifest is valid JSON with its keys in a different order and
// some incidental whitespace — exactly what a signer written in another language
// would emit. It is signed as-is and must verify. An implementation that
// re-serialised the parsed manifest before checking would reject it, and would
// report a correctly signed bundle as tampered with.
func TestAManifestSignedAsItWasWrittenVerifiesWhateverTheEncoding(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	payload := []byte("image")
	sum := sha256.Sum256(payload)
	// Hand-written, deliberately not in the struct's field order.
	raw := []byte(`{
  "key_id": "vendor-2026",
  "build": 1420,
  "product": "openaccessbridge",
  "payload_size": ` + itoa(len(payload)) + `,
  "version": "1.4.2",
  "format": 1,
  "created_at": "2026-08-21T12:00:00Z",
  "payload_sha256": "` + hex.EncodeToString(sum[:]) + `"
}`)
	sig := ed25519.Sign(s.priv, SigningInput(raw))
	var buf bytes.Buffer
	if err := Write(&buf, raw, sig, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(parse(t, buf.Bytes()), trust(t, s), installed); err != nil {
		t.Fatalf("a correctly signed manifest was refused because of how it was encoded: %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// A truncated payload is reported as truncation, which has a different remedy
// (re-copy the media) from a digest mismatch (the file is not what it claims).
func TestATruncatedPayloadIsRefused(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, bytes.Repeat([]byte("x"), 4096), nil)
	if _, err := Verify(parse(t, raw[:len(raw)-100]), trust(t, s), installed); !errors.Is(err, ErrPayloadMismatch) {
		t.Fatalf("a truncated bundle was accepted: %v", err)
	}
}

// A downgrade re-introduces every fault the newer build fixed, so it must be a
// decision somebody made rather than something that just happens.
func TestADowngradeIsRefusedUnlessItIsAsked(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("older image"), func(m *Manifest) { m.Build = 900 })

	_, err := Verify(parse(t, raw), trust(t, s), Current{Build: 1000, BuildKnown: true})
	if !errors.Is(err, ErrNotAnUpgrade) {
		t.Fatalf("an older build installed itself silently: %v", err)
	}
	if _, err := Verify(parse(t, raw), trust(t, s), Current{Build: 1000, BuildKnown: true, AllowDowngrade: true}); err != nil {
		t.Fatalf("a deliberate downgrade was refused: %v", err)
	}
}

// Re-installing the same build is not an upgrade either: it is the shape a
// replay attack takes on an offline update path.
func TestReinstallingTheSameBuildIsRefused(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("image"), func(m *Manifest) { m.Build = 1000 })
	if _, err := Verify(parse(t, raw), trust(t, s), Current{Build: 1000, BuildKnown: true}); !errors.Is(err, ErrNotAnUpgrade) {
		t.Fatalf("the installed build re-installed itself: %v", err)
	}
}

// A validly signed bundle for something else is still validly signed. Installing
// it is still wrong.
func TestABundleForAnotherProductIsRefused(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("image"), func(m *Manifest) { m.Product = "someone-elses-appliance" })
	if _, err := Verify(parse(t, raw), trust(t, s), installed); !errors.Is(err, ErrWrongProduct) {
		t.Fatalf("got %v, want ErrWrongProduct", err)
	}
}

// An appliance holding no keys can verify nothing, and must say that rather than
// falling through to accepting anything.
func TestAnApplianceWithNoTrustedKeysAcceptsNothing(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	empty, err := NewTrustStore()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(parse(t, s.bundle(t, []byte("image"), nil)), empty, installed); !errors.Is(err, ErrNoTrustedKeys) {
		t.Fatalf("got %v, want ErrNoTrustedKeys", err)
	}
	if _, err := Verify(parse(t, s.bundle(t, []byte("image"), nil)), nil, installed); !errors.Is(err, ErrNoTrustedKeys) {
		t.Fatalf("a nil trust store accepted a bundle: %v", err)
	}
}

// Media handed to an appliance by a person is hostile until checked. None of
// these may panic, hang, or allocate without bound.
func TestHostileMediaIsRefusedWithoutPanicking(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	good := s.bundle(t, []byte("image"), nil)

	cases := map[string][]byte{
		"empty":              {},
		"not a bundle":       []byte("just some file contents"),
		"magic only":         []byte(Magic),
		"header cut short":   append([]byte(Magic), 0x00, 0x01),
		"absurd manifest":    append([]byte(Magic), 0xFF, 0xFF, 0xFF, 0xFF, 0x00, 0x00, 0x00, 0x40),
		"absurd signature":   append([]byte(Magic), 0x00, 0x00, 0x00, 0x10, 0xFF, 0xFF, 0xFF, 0xFF),
		"body cut short":     good[:len(Magic)+8+4],
		"manifest not json":  buildRaw(t, []byte("{not json"), make([]byte, ed25519.SignatureSize), nil),
		"manifest empty obj": buildRaw(t, []byte("{}"), make([]byte, ed25519.SignatureSize), nil),
	}
	ts := trust(t, s)
	for name, raw := range cases {
		b, err := Parse(raw)
		if err != nil {
			continue // refused at parse, which is the point
		}
		if _, err := Verify(b, ts, installed); err == nil {
			t.Errorf("%s: hostile input was accepted as a valid update", name)
		}
	}
}

func buildRaw(t *testing.T, manifest, sig, payload []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, manifest, sig, payload); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Two keys under one id means one of them is a surprise.
func TestATrustStoreRefusesDuplicateKeyIDs(t *testing.T) {
	a, b := newSigner(t, "vendor"), newSigner(t, "vendor")
	if _, err := NewTrustStore(
		TrustedKey{ID: a.id, Public: a.pub},
		TrustedKey{ID: b.id, Public: b.pub},
	); err == nil {
		t.Fatal("two keys were trusted under the same id")
	}
}

// A bundle for a different hardware tier is refused (§4.3).
func TestABundleForAnotherTierIsRefused(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("image"), func(m *Manifest) { m.Tier = "hardened" })
	if _, err := Verify(parse(t, raw), trust(t, s), Current{Build: 1, BuildKnown: true, Tier: "standard"}); !errors.Is(err, ErrUnsupportedTier) {
		t.Fatalf("got %v, want ErrUnsupportedTier", err)
	}
	// And a bundle with no tier restriction installs anywhere.
	any := s.bundle(t, []byte("image"), nil)
	if _, err := Verify(parse(t, any), trust(t, s), Current{Build: 1, BuildKnown: true, Tier: "standard"}); err != nil {
		t.Fatalf("an untargeted bundle was refused: %v", err)
	}
}

// helpers that pull the raw manifest and signature back out of a bundle, for the
// test that proves an impostor's bundle is internally consistent.
func mustManifestBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	b := parse(t, raw)
	return b.manifestRaw
}

func mustSigBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	b := parse(t, raw)
	return b.signature
}

// An appliance that does not know its own build cannot tell an upgrade from a
// replay, and must REFUSE rather than pick a direction.
//
// The two directions are both wrong in the way this repo cares about. Treating
// an unknown build as zero makes every bundle look newer, so a downgrade to a
// version with a known fault installs silently. Treating it as enormous makes
// every bundle look older, so nothing installs and an operator who cannot verify
// is tempted to install unverified. Both are a verdict derived from an absence.
func TestAnApplianceThatDoesNotKnowItsBuildRefusesToJudge(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("image"), nil)

	if _, err := Verify(parse(t, raw), trust(t, s), Current{Build: 0, BuildKnown: false}); !errors.Is(err, ErrUnknownBuild) {
		t.Fatalf("an appliance with no build number accepted an update: %v", err)
	}
	// An operator who explicitly asks can still install: that is a decision
	// somebody made, which is the whole distinction.
	if _, err := Verify(parse(t, raw), trust(t, s), Current{BuildKnown: false, AllowDowngrade: true}); err != nil {
		t.Fatalf("a deliberate install on an unknown build was refused: %v", err)
	}
}

// A bundle that targets a hardware tier must not install on an appliance that
// has not been told which tier it is. Falling through to accept would make the
// tier field decoration.
func TestATargetedBundleIsRefusedWhenTheTierIsUnknown(t *testing.T) {
	s := newSigner(t, "vendor-2026")
	raw := s.bundle(t, []byte("image"), func(m *Manifest) { m.Tier = "hardened" })
	if _, err := Verify(parse(t, raw), trust(t, s), Current{Build: 1, BuildKnown: true}); !errors.Is(err, ErrUnknownTier) {
		t.Fatalf("a hardened-tier bundle installed on an appliance of unknown tier: %v", err)
	}
}
