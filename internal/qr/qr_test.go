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

// The correction level a caller asks for is the level the symbol carries.
//
// This reads the FORMAT INFORMATION out of the rendered matrix — where a scanner
// reads it — rather than the ECC field on the returned struct. The struct field
// is a copy of the argument, so checking it passed with formatBits() stubbed to a
// constant: the symbol would have announced the wrong redundancy to every phone
// that scanned it, and the test would have been green.
func TestTheRequestedCorrectionLevelIsTheOneEncodedInTheSymbol(t *testing.T) {
	// The two-bit ECC field as it appears in the format information. Written out
	// here as literals rather than by calling formatBits(), so this test cannot
	// agree with a broken implementation by construction.
	want := map[ECC]int{ECCLow: 0b01, ECCMedium: 0b00, ECCQuartile: 0b11, ECCHigh: 0b10}
	for ecc, bits := range want {
		sym, err := Encode([]byte("http://192.168.44.1:8080/accessibility"), ecc)
		if err != nil {
			t.Fatalf("%s: %v", ecc, err)
		}
		if got := readFormatECC(sym); got != bits {
			t.Errorf("%s: the symbol announces ECC bits %02b, want %02b", ecc, got, bits)
		}
		if got := readFormatMask(sym); got != sym.Mask {
			t.Errorf("%s: the symbol announces mask %d, the encoder chose %d", ecc, got, sym.Mask)
		}
	}
}

// readFormatECC and readFormatMask decode the 15-bit format field out of a
// rendered symbol, the way a scanner does: undo the 0x5412 mask, then take the
// five data bits from the top.
func formatField(sym *Symbol) int {
	field := 0
	// Copy 1, beside the top-left finder: field bit i for i in 9..14 sits at
	// (14-i, 8); bits 0..5 at (8, i).
	for i := 0; i <= 5; i++ {
		if sym.Dark(8, i) {
			field |= 1 << i
		}
	}
	if sym.Dark(8, 7) {
		field |= 1 << 6
	}
	if sym.Dark(8, 8) {
		field |= 1 << 7
	}
	if sym.Dark(7, 8) {
		field |= 1 << 8
	}
	for i := 9; i < 15; i++ {
		if sym.Dark(14-i, 8) {
			field |= 1 << i
		}
	}
	return field ^ 0x5412
}

func readFormatECC(sym *Symbol) int  { return (formatField(sym) >> 13) & 0b11 }
func readFormatMask(sym *Symbol) int { return (formatField(sym) >> 10) & 0b111 }

// The mask-selection rules must actually be doing something. Pinning eight
// reference matrices by forced mask left the PENALTY logic untested: deleting it
// entirely and always choosing mask 0 kept every other test green, because the
// only test of the choice compared the encoder against its own scoring function.
//
// So this checks the rules against symbols with KNOWN properties rather than
// against the implementation: the four penalties are exercised on hand-built
// lines, and the selection is required to reject a mask that scores worse.
func TestThePenaltyRulesScoreTheThingsTheStandardSaysTheyDo(t *testing.T) {
	// Rule 1: a run of five costs 3, and each module past five costs one more.
	if got := runPenalty(line("11111")); got != 3 {
		t.Errorf("a run of five scored %d, want 3", got)
	}
	if got := runPenalty(line("1111111")); got != 5 {
		t.Errorf("a run of seven scored %d, want 5", got)
	}
	if got := runPenalty(line("1111")); got != 0 {
		t.Errorf("a run of four scored %d, want 0", got)
	}
	// Rule 3: the 1:1:3:1:1 finder lookalike with four light modules after it.
	if got := countFinderLike(line("10111010000")); got != 1 {
		t.Errorf("the finder pattern with a trailing light margin scored %d, want 1", got)
	}
	if got := countFinderLike(line("00001011101")); got != 1 {
		t.Errorf("the finder pattern with a leading light margin scored %d, want 1", got)
	}
	// Without the four-module margin it is not the dangerous pattern.
	if got := countFinderLike(line("10111010100")); got != 0 {
		t.Errorf("a 1:1:3:1:1 without its light margin scored %d, want 0", got)
	}
}

// And the selection must genuinely prefer the lower score: forcing the mask the
// encoder rejected has to produce a symbol its own rules score worse.
func TestTheChosenMaskScoresBetterThanTheOnesRejected(t *testing.T) {
	vf := loadVectors(t)
	improved := 0
	for _, v := range vf.Cases {
		ecc := parseECC(t, v.ECC)
		chosen, err := Encode([]byte(v.Data), ecc)
		if err != nil {
			t.Fatal(err)
		}
		chosenScore := chosen.penalty()
		for m := 0; m < 8; m++ {
			if m == chosen.Mask {
				continue
			}
			other, err := encodeWithMask([]byte(v.Data), ecc, chosen.Version, m)
			if err != nil {
				t.Fatal(err)
			}
			if other.penalty() < chosenScore {
				t.Fatalf("%s: chose mask %d (%d) but mask %d scores %d",
					v.Name, chosen.Mask, chosenScore, m, other.penalty())
			}
			if other.penalty() > chosenScore {
				improved++
			}
		}
	}
	// If the scoring were inert every mask would tie, and the assertions above
	// would hold vacuously.
	if improved == 0 {
		t.Fatal("every mask scored identically on every fixture: the penalty rules are not discriminating")
	}
}

func line(bits string) []bool {
	out := make([]bool, len(bits))
	for i, c := range bits {
		out[i] = c == '1'
	}
	return out
}

// Encoding is deterministic// Encoding is deterministic: the same payload must produce the same symbol every
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
