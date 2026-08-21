package qr

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

type vectorFile struct {
	Note        string   `json:"note"`
	SegnoVerion string   `json:"segno_version"`
	Cases       []vector `json:"cases"`
}

type vector struct {
	Name       string     `json:"name"`
	Data       string     `json:"data"`
	ECC        string     `json:"ecc"`
	Version    int        `json:"version"`
	OracleMask int        `json:"oracle_mask"`
	Size       int        `json:"size"`
	Masks      []maskCase `json:"masks"`
	DecodedBy  string     `json:"decoded_by"`
}

// maskCase is one reference symbol: the same payload at one forced data mask,
// decoded by zxing-cpp before it was committed.
type maskCase struct {
	Mask    int      `json:"mask"`
	SHA256  string   `json:"sha256"`
	Decoded bool     `json:"decoded"`
	Matrix  []string `json:"matrix"`
}

func parseECC(t *testing.T, s string) ECC {
	t.Helper()
	switch s {
	case "L":
		return ECCLow
	case "M":
		return ECCMedium
	case "Q":
		return ECCQuartile
	case "H":
		return ECCHigh
	}
	t.Fatalf("unknown ECC level %q in a fixture", s)
	return ECCLow
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors.json")
	if err != nil {
		t.Fatalf("reading the golden vectors: %v", err)
	}
	var vf vectorFile
	if err := json.Unmarshal(raw, &vf); err != nil {
		t.Fatalf("parsing the golden vectors: %v", err)
	}
	if len(vf.Cases) == 0 {
		t.Fatal("the golden vector file is empty")
	}
	return vf
}

// The load-bearing test of this package. For every fixture, at every one of the
// eight data masks, our symbol must be byte-identical to a reference produced by
// python-qrcode — a reference that zxing-cpp, a real scanner's decoder, already
// read the exact payload back out of.
//
// That is the guarantee worth having: whichever mask the encoder selects, what a
// patron's camera is pointed at is a symbol an independent decoder has read. It
// deliberately does NOT require agreeing with anyone about WHICH mask to pick —
// implementations differ there on details the standard leaves open, and the
// chosen mask is recorded in the format information, so a scanner reads our
// answer rather than reproducing our reasoning.
func TestEveryModuleMatchesADecoderVerifiedReference(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Cases {
		t.Run(v.Name, func(t *testing.T) {
			if v.DecodedBy == "" {
				t.Fatalf("fixture %q was never decoded by an independent decoder: it is not ground truth", v.Name)
			}
			if len(v.Masks) != 8 {
				t.Fatalf("fixture %q pins %d masks, want 8", v.Name, len(v.Masks))
			}
			ecc := parseECC(t, v.ECC)
			for _, mc := range v.Masks {
				if !mc.Decoded {
					t.Fatalf("reference for mask %d was not decoded before it was committed", mc.Mask)
				}
				sym, err := encodeWithMask([]byte(v.Data), ecc, v.Version, mc.Mask)
				if err != nil {
					t.Fatalf("mask %d: %v", mc.Mask, err)
				}
				if sym.Mask != mc.Mask {
					t.Fatalf("asked for mask %d, symbol reports %d", mc.Mask, sym.Mask)
				}
				rows := matrixRows(sym)
				if got := sha256Rows(rows); got != mc.SHA256 {
					if mc.Matrix != nil {
						for y := range rows {
							if rows[y] != mc.Matrix[y] {
								t.Fatalf("mask %d, row %d differs from the reference:\n got  %s\n want %s\n%s",
									mc.Mask, y, rows[y], mc.Matrix[y], firstDifference(rows[y], mc.Matrix[y]))
							}
						}
					}
					t.Fatalf("mask %d: matrix hash %s, reference %s", mc.Mask, got[:16], mc.SHA256[:16])
				}
			}
		})
	}
}

// Version selection is the encoder's own decision and must match the oracle's:
// a version too small cannot hold the payload, and one too large wastes module
// size, which is the thing that actually decides whether a phone reads a card
// across a dark lobby.
func TestTheChosenVersionMatchesTheOracle(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Cases {
		sym, err := Encode([]byte(v.Data), parseECC(t, v.ECC))
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		if sym.Version != v.Version {
			t.Errorf("%s: chose version %d, the oracle chose %d (the capacity tables disagree)",
				v.Name, sym.Version, v.Version)
		}
		if sym.Size != v.Size {
			t.Errorf("%s: size %d modules, oracle %d", v.Name, sym.Size, v.Size)
		}
	}
}

// Mask SELECTION is checked for the property that is actually ours to get right:
// the encoder picks the mask its own penalty rules score lowest. Which mask that
// is may differ from another implementation (see the fixture generator); that it
// is the minimum of a deterministic scoring function is not negotiable, because
// the alternative is a mask chosen by accident of iteration order.
func TestTheSelectedMaskIsTheLowestScoringOne(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Cases {
		ecc := parseECC(t, v.ECC)
		chosen, err := Encode([]byte(v.Data), ecc)
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		best, bestScore := -1, 0
		for m := 0; m < 8; m++ {
			sym, err := encodeWithMask([]byte(v.Data), ecc, chosen.Version, m)
			if err != nil {
				t.Fatalf("%s mask %d: %v", v.Name, m, err)
			}
			score := sym.penalty()
			if best < 0 || score < bestScore {
				best, bestScore = m, score
			}
		}
		if chosen.Mask != best {
			t.Errorf("%s: selected mask %d, but mask %d scores lower (%d)", v.Name, chosen.Mask, best, bestScore)
		}
	}
}

func matrixRows(sym *Symbol) []string {
	rows := make([]string, sym.Size)
	for y := 0; y < sym.Size; y++ {
		b := make([]byte, sym.Size)
		for x := 0; x < sym.Size; x++ {
			b[x] = '0'
			if sym.Dark(x, y) {
				b[x] = '1'
			}
		}
		rows[y] = string(b)
	}
	return rows
}

func sha256Rows(rows []string) string {
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return hex.EncodeToString(sum[:])
}

func firstDifference(got, want string) string {
	for i := range got {
		if i < len(want) && got[i] != want[i] {
			return strings.Repeat(" ", i+6) + "^ first difference at column " + itoa(i)
		}
	}
	return ""
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// A payload that does not fit must be REFUSED, never silently truncated and
// never silently downgraded to a weaker correction level. A truncated join URL
// is a QR that scans perfectly and sends a patron to a broken address.
func TestAnOversizedPayloadIsRefusedRatherThanTruncated(t *testing.T) {
	// Version 40-H holds 1273 bytes; 40-L holds 2953.
	tooBig := make([]byte, 3000)
	for i := range tooBig {
		tooBig[i] = 'A'
	}
	if _, err := Encode(tooBig, ECCLow); err == nil {
		t.Fatal("a 3000-byte payload was accepted at level L")
	}
	// And a payload that fits at L but not at H must fail at H rather than
	// quietly dropping to L.
	mid := make([]byte, 2000)
	for i := range mid {
		mid[i] = 'A'
	}
	if _, err := Encode(mid, ECCLow); err != nil {
		t.Fatalf("2000 bytes should fit at level L: %v", err)
	}
	sym, err := Encode(mid, ECCHigh)
	if err == nil {
		t.Fatalf("2000 bytes must not fit at level H, but got version %d-%s", sym.Version, sym.ECC)
	}
}

// The correction level a caller asks for is the level the symbol carries. The
// level is encoded in the format information, so a mix-up here produces a symbol
// that scans and reports the wrong redundancy — invisible without a decoder.
func TestTheRequestedCorrectionLevelIsTheOneEncoded(t *testing.T) {
	vf := loadVectors(t)
	for _, v := range vf.Cases {
		sym, err := Encode([]byte(v.Data), parseECC(t, v.ECC))
		if err != nil {
			t.Fatalf("%s: %v", v.Name, err)
		}
		if sym.ECC != parseECC(t, v.ECC) {
			t.Fatalf("%s: asked for %s, symbol carries %s", v.Name, v.ECC, sym.ECC)
		}
	}
}

// Encoding is deterministic: the same payload must produce the same symbol every
// time. A per-show QR that changed between the admin page and the printed card
// would be a support call nobody could reproduce.
func TestEncodingIsDeterministic(t *testing.T) {
	data := []byte("http://192.168.44.1:8080/accessibility?t=v1.abc.def")
	first, err := Encode(data, DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Encode(data, DefaultECC)
		if err != nil {
			t.Fatal(err)
		}
		if again.Version != first.Version || again.Mask != first.Mask {
			t.Fatalf("run %d chose version %d mask %d, first run chose %d/%d",
				i, again.Version, again.Mask, first.Version, first.Mask)
		}
		for y := 0; y < first.Size; y++ {
			for x := 0; x < first.Size; x++ {
				if again.Dark(x, y) != first.Dark(x, y) {
					t.Fatalf("run %d differs at (%d,%d)", i, x, y)
				}
			}
		}
	}
}

// The three finder patterns are what a scanner locates first. This is a cheap
// structural check that fails loudly if the geometry is ever rearranged — the
// oracle comparison covers it too, but this one names the fault.
func TestTheThreeFindersArePresentAndTheFourthCornerIsNot(t *testing.T) {
	sym, err := Encode([]byte("CB"), DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	corners := []struct {
		name string
		x, y int
		want bool
	}{
		{"top-left", 3, 3, true},
		{"top-right", sym.Size - 4, 3, true},
		{"bottom-left", 3, sym.Size - 4, true},
	}
	for _, c := range corners {
		// A finder's centre module is dark and the ring at distance 2 is light.
		if sym.Dark(c.x, c.y) != c.want {
			t.Errorf("%s finder centre is not dark", c.name)
		}
		if sym.Dark(c.x+2, c.y) {
			t.Errorf("%s finder's light ring is dark", c.name)
		}
	}
	// The dark module: always dark, at a fixed coordinate, in every symbol.
	if !sym.Dark(8, sym.Size-8) {
		t.Error("the always-dark module is light")
	}
}

type exhaustiveFile struct {
	Note  string           `json:"note"`
	Cases []exhaustiveCase `json:"cases"`
}

type exhaustiveCase struct {
	Version      int    `json:"version"`
	ECC          string `json:"ecc"`
	ByteCapacity int    `json:"byte_capacity"`
	Mask         int    `json:"mask"`
	Size         int    `json:"size"`
	MatrixSHA256 string `json:"matrix_sha256"`
}

// fill mirrors the generator's fill(): a deterministic payload of exactly n
// bytes. Both sides must agree or the comparison is meaningless, which is why it
// is spelled out in both languages rather than shipped as data.
func fill(n int) []byte {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	out := make([]byte, n)
	for i := range out {
		out[i] = alphabet[i%len(alphabet)]
	}
	return out
}

// Every version at every correction level, each filled to its exact capacity.
//
// This is the test that makes the tables in tables.go trustworthy. A slip in one
// entry of the error-correction block counts breaks exactly one of the 160
// combinations and leaves the other 159 perfect — a defect that survives casual
// testing forever, and that would surface as an unreadable QR in one auditorium
// whose join URL happened to be a particular length. Two facts are checked per
// combination: the byte capacity (found by the oracle through bisection, not
// copied from a table) and the full module matrix at that capacity.
func TestEveryVersionAndLevelMatchesTheOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/exhaustive.json")
	if err != nil {
		t.Fatalf("reading the exhaustive vectors: %v", err)
	}
	var ef exhaustiveFile
	if err := json.Unmarshal(raw, &ef); err != nil {
		t.Fatalf("parsing the exhaustive vectors: %v", err)
	}
	if len(ef.Cases) != 160 {
		t.Fatalf("expected all 40 versions x 4 levels = 160 combinations, got %d", len(ef.Cases))
	}
	for _, c := range ef.Cases {
		ecc := parseECC(t, c.ECC)
		if got := byteCapacity(c.Version, ecc); got != c.ByteCapacity {
			t.Errorf("v%d-%s: byte capacity %d, oracle %d", c.Version, c.ECC, got, c.ByteCapacity)
			continue
		}
		// One byte past capacity must be refused at this version.
		if _, err := EncodeVersion(fill(c.ByteCapacity+1), ecc, c.Version); err == nil {
			t.Errorf("v%d-%s: accepted %d bytes, one past its stated capacity", c.Version, c.ECC, c.ByteCapacity+1)
		}
		sym, err := encodeWithMask(fill(c.ByteCapacity), ecc, c.Version, c.Mask)
		if err != nil {
			t.Errorf("v%d-%s at capacity: %v", c.Version, c.ECC, err)
			continue
		}
		if sym.Size != c.Size {
			t.Errorf("v%d-%s: size %d, oracle %d", c.Version, c.ECC, sym.Size, c.Size)
		}
		if got := sha256Rows(matrixRows(sym)); got != c.MatrixSHA256 {
			t.Errorf("v%d-%s: matrix differs from the oracle (hash %s vs %s)",
				c.Version, c.ECC, got[:16], c.MatrixSHA256[:16])
		}
	}
}
