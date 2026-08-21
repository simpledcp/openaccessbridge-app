// Package update implements §9.4's signed, offline-capable update path and the
// A/B slot model with automatic rollback.
//
// §9.4: "The primary update path is signed and offline-capable (e.g., a signed
// bundle applied from local media), verified against a key held in the
// TPM/secure storage on the hardened tier. Cloud update is optional and
// outbound-only. The hardened tier uses A/B partitions for atomic updates with
// automatic rollback on failed boot/health-check, so a bad update never bricks a
// booth appliance."
//
// THE APPLIANCE HOLDS PUBLIC KEYS ONLY, and that is the load-bearing decision in
// this file. It verifies signatures; it cannot make them. An appliance that
// could sign an update it would then accept is an appliance whose compromise is
// permanent — one write to its trust store and every future update is whatever
// the attacker wants. The private half lives wherever the vendor keeps it (a
// TPM or an HSM on the hardened tier, §4.3), which is off this box by
// definition.
//
// THE OTHER HALF OF THE SAME RULE: the trusted key is never taken from the
// bundle. A bundle that carries its own key and is checked against it verifies
// perfectly and proves nothing, which is the single most common way a signed
// update path turns out not to be one. Verify takes its trust store from the
// caller and matches by key id; a bundle naming a key the appliance does not
// hold is refused, not trusted.
//
// WHAT IS NOT HERE, and is recorded in BLOCKERS B8: this image has no A/B
// partitions, no bootloader integration, no secure boot and no TPM. The slot
// model in slots.go is a complete and tested state machine; what it cannot do on
// this host is make a machine boot from the other slot. That seam refuses rather
// than pretending.
package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Magic identifies an OpenAccessBridge update bundle and its format version. It
// checked first so a file that is not a bundle at all fails with that reason
// rather than as a parse error deeper in.
const Magic = "OABUPD1\n"

// MaxManifestBytes bounds the manifest. It is read from removable media handed
// to the appliance by a person, so every length that comes off that media is
// treated as hostile until it has been checked.
const MaxManifestBytes = 1 << 16

// MaxPayloadBytes bounds the payload at 512 MiB. An appliance image is a long
// way under that; the bound exists so a malformed or malicious length cannot ask
// the appliance to allocate until it dies.
const MaxPayloadBytes = 512 << 20

// Refusals. Each one is a distinct reason, because "the update was rejected" is
// not something an operator standing at a booth with a USB stick can act on.
var (
	ErrNotABundle      = errors.New("update: this file is not an OpenAccessBridge update bundle")
	ErrMalformed       = errors.New("update: the bundle is malformed")
	ErrUnknownKey      = errors.New("update: the bundle is signed by a key this appliance does not trust")
	ErrBadSignature    = errors.New("update: the signature does not verify")
	ErrPayloadMismatch = errors.New("update: the payload does not match the digest the manifest signs")
	ErrWrongProduct    = errors.New("update: this bundle is not for OpenAccessBridge")
	ErrNotAnUpgrade    = errors.New("update: this bundle is not newer than the version already installed")
	ErrTooLarge        = errors.New("update: the bundle declares a length this appliance will not read")
	ErrNoTrustedKeys   = errors.New("update: this appliance holds no trusted update keys, so no update can be verified")
	ErrUnsupportedTier = errors.New("update: this bundle targets a hardware tier this appliance is not")
	// ErrUnknownBuild: this appliance does not know its own build number, so it
	// cannot tell an upgrade from a replay. Refusing beats picking a direction:
	// treating an unknown build as zero installs every downgrade silently, and
	// treating it as enormous installs nothing. Both are verdicts derived from an
	// absence.
	ErrUnknownBuild = errors.New("update: this appliance does not know its own build number, so it cannot tell an upgrade from a replay")
	// ErrUnknownTier: the bundle targets a specific hardware tier and this
	// appliance has not been told which tier it is. An untargeted bundle still
	// installs anywhere; only a targeted one needs the answer.
	ErrUnknownTier = errors.New("update: this bundle targets a hardware tier and this appliance has not been told which tier it is")
)

// Product is the only product string this appliance will install.
const Product = "openaccessbridge"

// Manifest describes an update. It is the SIGNED part: the payload is bound to
// it by digest, so one signature covers both without the signer having to stream
// the whole image.
type Manifest struct {
	// Format is the manifest schema version, separate from Magic's bundle format.
	Format int `json:"format"`
	// Product must be "openaccessbridge". A signed bundle for something else is
	// still a validly signed bundle, and installing it is still wrong.
	Product string `json:"product"`
	// Version is the human-readable release, e.g. "1.4.2".
	Version string `json:"version"`
	// Build is the MONOTONIC ordering. Version strings are for people; comparing
	// them is a source of bugs (is "1.10" newer than "1.9"?), so ordering is an
	// integer the release process owns.
	Build int64 `json:"build"`
	// Tier optionally restricts the bundle to a hardware tier (§4.3). Empty means
	// any tier.
	Tier string `json:"tier,omitempty"`
	// CreatedAt is informational; nothing is decided by it. A clock-based
	// acceptance rule on an appliance with no reliable time source would be a
	// denial of service waiting to happen.
	CreatedAt time.Time `json:"created_at"`
	// PayloadSHA256 and PayloadSize bind the payload. The size is checked as well
	// as the digest so a truncated read fails as truncation rather than as a
	// digest mismatch, which is a different fault with a different remedy.
	PayloadSHA256 string `json:"payload_sha256"`
	PayloadSize   int64  `json:"payload_size"`
	// KeyID selects which trusted key should verify this bundle. It is a HINT for
	// lookup, never a grant: the key itself must already be in the appliance's
	// trust store.
	KeyID string `json:"key_id"`
	// Notes is release text shown to an operator. It is never executed, never
	// used in a path, and never trusted for anything.
	Notes string `json:"notes,omitempty"`
}

// TrustedKey is one update-signing key the appliance holds.
type TrustedKey struct {
	ID string
	// Public is the ed25519 public key. There is deliberately no field for a
	// private key anywhere in this package.
	Public ed25519.PublicKey
}

// TrustStore is the set of keys this appliance will accept updates from.
type TrustStore struct {
	keys map[string]ed25519.PublicKey
}

// NewTrustStore builds a trust store. Duplicate ids are an error rather than a
// last-one-wins: two keys under one name means one of them is a surprise.
func NewTrustStore(keys ...TrustedKey) (*TrustStore, error) {
	ts := &TrustStore{keys: map[string]ed25519.PublicKey{}}
	for _, k := range keys {
		id := strings.TrimSpace(k.ID)
		if id == "" {
			return nil, errors.New("update: a trusted key needs an id")
		}
		if len(k.Public) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("update: trusted key %q is not an ed25519 public key", id)
		}
		if _, dup := ts.keys[id]; dup {
			return nil, fmt.Errorf("update: two trusted keys share the id %q", id)
		}
		ts.keys[id] = k.Public
	}
	return ts, nil
}

// Len is how many keys are trusted.
func (ts *TrustStore) Len() int {
	if ts == nil {
		return 0
	}
	return len(ts.keys)
}

// lookup finds a key by id.
func (ts *TrustStore) lookup(id string) (ed25519.PublicKey, bool) {
	if ts == nil {
		return nil, false
	}
	k, ok := ts.keys[id]
	return k, ok
}

// Bundle is a parsed, NOT YET VERIFIED bundle. The type is separate from
// VerifiedBundle so that no code path can hold something that looks like an
// update without having verified it: parsing gives you a Bundle, and only Verify
// gives you the thing that can be installed.
type Bundle struct {
	Manifest Manifest
	// manifestRaw is the exact bytes the signature covers.
	//
	// A signature covers BYTES, so verification is done over the bytes that
	// arrived. Re-serialising the parsed manifest and verifying that instead
	// would tie this appliance to one JSON encoder's field order, spacing and
	// escaping: a signer that emitted the same manifest with the keys in a
	// different order would produce bundles this appliance rejected, and the
	// failure would present as "bad signature" — which is what an operator is
	// told when a bundle has been tampered with. Being unable to tell a
	// mis-encoded bundle from a hostile one is the fault worth avoiding here.
	manifestRaw []byte
	signature   []byte
	payload     []byte
}

// VerifiedBundle is a bundle whose signature, digest and applicability have all
// been checked. Nothing in this package installs anything without one.
type VerifiedBundle struct {
	Manifest Manifest
	// KeyID is the trusted key that actually verified it, so the audit record
	// names the key rather than "a signature".
	KeyID   string
	payload []byte
}

// Payload returns the verified image bytes.
func (v VerifiedBundle) Payload() []byte { return v.payload }

// Write serialises a bundle. It is used by the signing tool and by tests; it
// takes a signature rather than making one, because nothing in this package
// holds a private key.
func Write(w io.Writer, manifestRaw, signature, payload []byte) error {
	if _, err := io.WriteString(w, Magic); err != nil {
		return err
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(manifestRaw)))
	binary.BigEndian.PutUint32(hdr[4:8], uint32(len(signature)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := w.Write(manifestRaw); err != nil {
		return err
	}
	if _, err := w.Write(signature); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// Parse reads a bundle without verifying anything about it.
func Parse(raw []byte) (*Bundle, error) {
	if len(raw) < len(Magic)+8 || string(raw[:len(Magic)]) != Magic {
		return nil, ErrNotABundle
	}
	rest := raw[len(Magic):]
	manifestLen := binary.BigEndian.Uint32(rest[0:4])
	sigLen := binary.BigEndian.Uint32(rest[4:8])
	rest = rest[8:]

	if manifestLen > MaxManifestBytes {
		return nil, fmt.Errorf("%w: manifest is %d bytes", ErrTooLarge, manifestLen)
	}
	if sigLen != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature is %d bytes, want %d", ErrMalformed, sigLen, ed25519.SignatureSize)
	}
	if uint64(len(rest)) < uint64(manifestLen)+uint64(sigLen) {
		return nil, fmt.Errorf("%w: the bundle ends inside its own header", ErrMalformed)
	}
	manifestRaw := rest[:manifestLen]
	signature := rest[manifestLen : manifestLen+uint32(sigLen)]
	payload := rest[manifestLen+uint32(sigLen):]
	if int64(len(payload)) > MaxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes", ErrTooLarge, len(payload))
	}

	var m Manifest
	if err := json.Unmarshal(manifestRaw, &m); err != nil {
		return nil, fmt.Errorf("%w: manifest is not valid JSON: %v", ErrMalformed, err)
	}
	return &Bundle{
		Manifest:    m,
		manifestRaw: append([]byte(nil), manifestRaw...),
		signature:   append([]byte(nil), signature...),
		payload:     append([]byte(nil), payload...),
	}, nil
}

// Current describes what is installed, for the upgrade check.
type Current struct {
	// Build is the installed build number, and BuildKnown says whether this
	// appliance knows it at all. They are separate because "build 0" and "we do
	// not know our build" lead to opposite decisions, and only one of them is a
	// fact.
	Build      int64
	BuildKnown bool
	Tier       string
	// AllowDowngrade lets an operator install an older build deliberately. It is
	// off by default and is never inferred: a downgrade re-introduces every fault
	// the newer build fixed, so it has to be a decision somebody made.
	AllowDowngrade bool
}

// Verify checks a bundle against the trust store and the installed version, and
// returns something installable only if every check passes.
//
// The order matters and is deliberate: cheap structural checks first, then the
// signature, then the payload digest, then applicability. The signature is
// checked BEFORE the payload is hashed so a bundle nobody trusted cannot make
// the appliance do work proportional to its size, and applicability is checked
// last so an operator is told "not newer" rather than "bad signature" when the
// bundle is genuinely ours.
func Verify(b *Bundle, ts *TrustStore, cur Current) (VerifiedBundle, error) {
	if b == nil {
		return VerifiedBundle{}, ErrMalformed
	}
	if ts.Len() == 0 {
		return VerifiedBundle{}, ErrNoTrustedKeys
	}
	m := b.Manifest
	if m.Format != 1 {
		return VerifiedBundle{}, fmt.Errorf("%w: manifest format %d", ErrMalformed, m.Format)
	}
	if m.Product != Product {
		return VerifiedBundle{}, fmt.Errorf("%w: %q", ErrWrongProduct, m.Product)
	}
	if strings.TrimSpace(m.Version) == "" || m.Build <= 0 {
		return VerifiedBundle{}, fmt.Errorf("%w: manifest has no version or build", ErrMalformed)
	}

	// The key is looked up in the appliance's OWN trust store. The bundle names
	// which key; it never supplies one.
	pub, ok := ts.lookup(m.KeyID)
	if !ok {
		return VerifiedBundle{}, fmt.Errorf("%w: key id %q", ErrUnknownKey, m.KeyID)
	}
	// Verified over the EXACT manifest bytes that were read, not over a
	// re-serialisation of the parsed struct.
	if !ed25519.Verify(pub, b.manifestRaw, b.signature) {
		return VerifiedBundle{}, ErrBadSignature
	}

	if int64(len(b.payload)) != m.PayloadSize {
		return VerifiedBundle{}, fmt.Errorf("%w: payload is %d bytes, the manifest signs %d",
			ErrPayloadMismatch, len(b.payload), m.PayloadSize)
	}
	sum := sha256.Sum256(b.payload)
	if !bytes.Equal(sum[:], mustHex(m.PayloadSHA256)) {
		return VerifiedBundle{}, fmt.Errorf("%w: digest %s, the manifest signs %s",
			ErrPayloadMismatch, hex.EncodeToString(sum[:]), m.PayloadSHA256)
	}

	if m.Tier != "" {
		switch {
		case cur.Tier == "":
			return VerifiedBundle{}, fmt.Errorf("%w: bundle targets %q", ErrUnknownTier, m.Tier)
		case !strings.EqualFold(m.Tier, cur.Tier):
			return VerifiedBundle{}, fmt.Errorf("%w: bundle targets %q, this appliance is %q",
				ErrUnsupportedTier, m.Tier, cur.Tier)
		}
	}
	if !cur.BuildKnown && !cur.AllowDowngrade {
		return VerifiedBundle{}, ErrUnknownBuild
	}
	if m.Build <= cur.Build && !cur.AllowDowngrade {
		return VerifiedBundle{}, fmt.Errorf("%w: bundle build %d, installed build %d",
			ErrNotAnUpgrade, m.Build, cur.Build)
	}

	return VerifiedBundle{Manifest: m, KeyID: m.KeyID, payload: b.payload}, nil
}

// mustHex decodes a hex digest, returning nil for anything malformed so the
// comparison fails rather than panicking on operator-supplied media.
func mustHex(s string) []byte {
	out, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil
	}
	return out
}

// SigningInput returns the exact bytes a signer must sign for a manifest. It
// exists so a signing tool and this verifier cannot disagree about what is
// covered — the commonest way a signature ends up over the wrong thing.
func SigningInput(manifestRaw []byte) []byte { return manifestRaw }
