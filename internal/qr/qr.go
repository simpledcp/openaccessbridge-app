// Package qr encodes QR Code Model 2 symbols (ISO/IEC 18004) in byte mode.
//
// WHY IT IS IN THE TREE. §7.2 and §10.4 want a per-show QR so a patron joins in
// one scan, and §9.5 wants onboarding to work with no internet at all. An
// appliance that ships as one static binary (D2, D4) cannot render a QR by
// calling a service, and the alternative — a module dependency — buys a few
// hundred lines at the cost of a supply-chain surface on a box that is
// deliberately offline. This is a closed, frozen, fully specified format: it
// will not need updating.
//
// HOW IT IS KNOWN TO BE CORRECT — which is the entire problem with shipping a
// QR. A bitmap nobody can read is the perfect place for a bug to hide: it looks
// like a QR, it is square, it has three corners, and it fails silently at the
// auditorium door in a patron's hands. An earlier pass refused to render one for
// exactly this reason (the note is still in internal/web/session_admin.go).
//
// So nothing here is checked against our own understanding of the spec. Every
// module of every symbol is compared against a reference produced by
// python-qrcode — an unrelated implementation — and every one of those
// references was decoded by zxing-cpp, the C++ ZXing engine behind a great many
// phone scanner apps, which had to return the exact payload before the fixture
// was committed. The comparison covers all eight data masks for a corpus of
// boundary cases AND all 160 version/correction-level combinations at their
// exact capacity, so the spec tables in tables.go are pinned rather than
// spot-checked. See testdata/generate_vectors.py, which also records what
// happened when a third implementation disagreed.
//
// SCOPE, deliberately narrow: Model 2 symbols, byte mode (UTF-8), ECC levels
// L/M/Q/H, versions 1-40, automatic version and mask selection. No Micro QR, no
// kanji/alphanumeric/numeric mode, no structured append, no ECI. A join URL is
// mixed-case with a base64 token in it, so alphanumeric mode cannot carry it and
// the compaction the other modes buy is worth nothing here.
package qr

import (
	"errors"
	"fmt"
)

// ECC is a QR error-correction level: how much of the symbol is redundancy, and
// therefore how much damage or glare a scan survives.
type ECC int

const (
	// ECCLow recovers ~7% of codewords.
	ECCLow ECC = iota
	// ECCMedium recovers ~15%. This is the appliance's default: see
	// DefaultECC.
	ECCMedium
	// ECCQuartile recovers ~25%.
	ECCQuartile
	// ECCHigh recovers ~30%.
	ECCHigh
)

// DefaultECC is the level the appliance renders join codes at.
//
// The trade is not "more correction is better". A higher level needs more
// codewords, which means a higher version, which means MORE AND SMALLER MODULES
// at a given physical size — and the thing that actually defeats scans at a
// cinema door is small modules in bad light, not codeword corruption. A typical
// §10.4 join URL is 53x53 modules at Medium and 61x61 at Quartile: at the same
// printed width every module is 15% smaller for correction a clean screen or a
// fresh card does not need.
const DefaultECC = ECCMedium

func (e ECC) String() string {
	switch e {
	case ECCLow:
		return "L"
	case ECCMedium:
		return "M"
	case ECCQuartile:
		return "Q"
	case ECCHigh:
		return "H"
	default:
		return fmt.Sprintf("ECC(%d)", int(e))
	}
}

// formatBits is the two-bit ECC field as it appears in the format information.
// The order is NOT the numeric order of the levels (L=01, M=00, Q=11, H=10),
// which is a classic silent-corruption bug: the symbol scans, and reports the
// wrong level.
func (e ECC) formatBits() int {
	switch e {
	case ECCLow:
		return 0b01
	case ECCMedium:
		return 0b00
	case ECCQuartile:
		return 0b11
	default:
		return 0b10
	}
}

// ErrTooLong is returned when the payload does not fit in any version at the
// requested level. It is an error rather than a silent downgrade of the ECC
// level: a caller that asked for correction gets what it asked for or is told.
var ErrTooLong = errors.New("qr: payload does not fit in a version 40 symbol at this correction level")

// Symbol is a rendered QR matrix. It carries the version and mask it chose so a
// test — and a bug report — can name them.
type Symbol struct {
	// Version is 1-40; Size is 4*Version+17 modules on a side.
	Version int
	// ECC is the level actually applied.
	ECC ECC
	// Mask is the data mask pattern chosen (0-7) by the spec's penalty rules.
	Mask int
	// Size is the side length in modules, excluding the quiet zone.
	Size int

	modules []bool // Size*Size, true = dark
	fixed   []bool // Size*Size, true = function pattern (never masked)
}

// Dark reports whether the module at (x, y) is dark. Out-of-range coordinates
// are light, which is what the quiet zone is.
func (s *Symbol) Dark(x, y int) bool {
	if x < 0 || y < 0 || x >= s.Size || y >= s.Size {
		return false
	}
	return s.modules[y*s.Size+x]
}

func (s *Symbol) set(x, y int, dark bool) { s.modules[y*s.Size+x] = dark }

func (s *Symbol) setFunction(x, y int, dark bool) {
	s.modules[y*s.Size+x] = dark
	s.fixed[y*s.Size+x] = true
}

// Encode builds the smallest symbol that holds data at the given level.
func Encode(data []byte, ecc ECC) (*Symbol, error) {
	version, err := smallestVersion(len(data), ecc)
	if err != nil {
		return nil, err
	}
	return EncodeVersion(data, ecc, version)
}

// EncodeVersion builds a symbol at an explicit version, for callers that need a
// stable physical size across shows (a printed card holder) rather than the
// smallest symbol. It fails rather than overflowing.
func EncodeVersion(data []byte, ecc ECC, version int) (*Symbol, error) {
	if version < 1 || version > 40 {
		return nil, fmt.Errorf("qr: version %d out of range 1-40", version)
	}
	if capacity := byteCapacity(version, ecc); len(data) > capacity {
		return nil, fmt.Errorf("qr: %d bytes exceeds the %d-byte capacity of version %d-%s: %w",
			len(data), capacity, version, ecc, ErrTooLong)
	}
	bits := encodeSegment(data, version)
	codewords := addTerminatorAndPad(bits, dataCodewords(version, ecc))
	final := interleave(codewords, version, ecc)

	size := version*4 + 17
	s := &Symbol{Version: version, ECC: ecc, Size: size,
		modules: make([]bool, size*size), fixed: make([]bool, size*size)}
	s.drawFunctionPatterns()
	s.drawCodewords(final)
	s.applyBestMask()
	return s, nil
}

// encodeSegment writes the byte-mode header and payload as a bit stream.
func encodeSegment(data []byte, version int) *bitBuffer {
	b := &bitBuffer{}
	b.append(0b0100, 4) // byte mode
	b.append(len(data), byteCountBits(version))
	for _, c := range data {
		b.append(int(c), 8)
	}
	return b
}

// byteCountBits is the width of the character-count indicator in byte mode. It
// widens twice as versions grow; using the wrong width shifts every subsequent
// bit and produces a symbol that decodes to nothing.
func byteCountBits(version int) int {
	switch {
	case version <= 9:
		return 8
	default:
		return 16
	}
}

// addTerminatorAndPad closes the bit stream and fills the remaining data
// capacity with the spec's alternating pad bytes.
func addTerminatorAndPad(b *bitBuffer, dataCodewords int) []byte {
	capacityBits := dataCodewords * 8
	// Terminator: up to four zero bits, fewer if the stream nearly fills the
	// capacity.
	term := 4
	if remaining := capacityBits - b.len(); remaining < term {
		term = remaining
	}
	b.append(0, term)
	// Pad to a byte boundary.
	if pad := (8 - b.len()%8) % 8; pad > 0 {
		b.append(0, pad)
	}
	out := b.bytes()
	for i := 0; len(out) < dataCodewords; i++ {
		if i%2 == 0 {
			out = append(out, 0xEC)
		} else {
			out = append(out, 0x11)
		}
	}
	return out
}

// interleave splits the data codewords into the version/level's error-correction
// blocks, computes each block's Reed-Solomon codewords, and interleaves both
// sets in the order the spec lays them into the symbol.
//
// Blocks come in at most two sizes, and the short ones come first. Getting the
// split wrong produces a symbol that still scans on a clean read and fails as
// soon as error correction has to do anything — the worst possible failure, in
// that it would pass every bench test and fail in a dark auditorium.
func interleave(data []byte, version int, ecc ECC) []byte {
	numBlocks := numBlocks(version, ecc)
	ecPerBlock := ecCodewordsPerBlock(version, ecc)
	total := totalCodewords(version)
	shortLen := (total / numBlocks) - ecPerBlock
	numShort := numBlocks - total%numBlocks

	blocks := make([][]byte, numBlocks)
	ecBlocks := make([][]byte, numBlocks)
	gen := rsGenerator(ecPerBlock)
	off := 0
	for i := 0; i < numBlocks; i++ {
		n := shortLen
		if i >= numShort {
			n = shortLen + 1
		}
		blocks[i] = data[off : off+n]
		off += n
		ecBlocks[i] = rsRemainder(blocks[i], gen)
	}

	out := make([]byte, 0, total)
	for i := 0; i < shortLen+1; i++ {
		for b := range blocks {
			if i < len(blocks[b]) {
				out = append(out, blocks[b][i])
			}
		}
	}
	for i := 0; i < ecPerBlock; i++ {
		for b := range ecBlocks {
			out = append(out, ecBlocks[b][i])
		}
	}
	return out
}

// drawCodewords places the final codeword stream in the two-module-wide
// serpentine the spec defines, skipping function patterns and the vertical
// timing column.
func (s *Symbol) drawCodewords(data []byte) {
	i := 0 // bit index
	upward := true
	for right := s.Size - 1; right >= 1; right -= 2 {
		if right == 6 {
			// Column 6 is the vertical timing pattern; the pairs step around it.
			right = 5
		}
		for vert := 0; vert < s.Size; vert++ {
			y := vert
			if upward {
				y = s.Size - 1 - vert
			}
			for x := right; x > right-2; x-- {
				if s.fixed[y*s.Size+x] {
					continue
				}
				dark := false
				if i>>3 < len(data) {
					dark = (data[i>>3]>>(7-uint(i&7)))&1 == 1
				}
				// Bits past the end of the data are the remainder bits, which
				// are light by definition.
				s.set(x, y, dark)
				i++
			}
		}
		upward = !upward
	}
}

// applyBestMask evaluates all eight data masks under the spec's four penalty
// rules and keeps the lowest-scoring one. The rules exist to avoid patterns a
// scanner could mistake for a finder, so this is not cosmetic: a badly masked
// symbol is a symbol some phones cannot read.
func (s *Symbol) applyBestMask() {
	best, bestScore := 0, -1
	for mask := 0; mask < 8; mask++ {
		s.applyMask(mask)
		s.drawFormatInfo(mask)
		score := s.penalty()
		if bestScore < 0 || score < bestScore {
			best, bestScore = mask, score
		}
		s.applyMask(mask) // XOR is its own inverse: undo
	}
	s.applyMask(best)
	s.drawFormatInfo(best)
	s.Mask = best
}

func (s *Symbol) applyMask(mask int) {
	for y := 0; y < s.Size; y++ {
		for x := 0; x < s.Size; x++ {
			if s.fixed[y*s.Size+x] {
				continue
			}
			if maskBit(mask, x, y) {
				s.modules[y*s.Size+x] = !s.modules[y*s.Size+x]
			}
		}
	}
}

func maskBit(mask, x, y int) bool {
	switch mask {
	case 0:
		return (x+y)%2 == 0
	case 1:
		return y%2 == 0
	case 2:
		return x%3 == 0
	case 3:
		return (x+y)%3 == 0
	case 4:
		return (y/2+x/3)%2 == 0
	case 5:
		return x*y%2+x*y%3 == 0
	case 6:
		return (x*y%2+x*y%3)%2 == 0
	default:
		return ((x+y)%2+x*y%3)%2 == 0
	}
}

// bitBuffer accumulates a big-endian bit stream.
type bitBuffer struct {
	bits []bool
}

func (b *bitBuffer) append(value, width int) {
	for i := width - 1; i >= 0; i-- {
		b.bits = append(b.bits, (value>>uint(i))&1 == 1)
	}
}

func (b *bitBuffer) len() int { return len(b.bits) }

func (b *bitBuffer) bytes() []byte {
	out := make([]byte, (len(b.bits)+7)/8)
	for i, bit := range b.bits {
		if bit {
			out[i>>3] |= 1 << (7 - uint(i&7))
		}
	}
	return out
}

// encodeWithMask builds a symbol at an explicit version AND data mask, skipping
// the penalty-based selection. It exists for the tests, which pin our output
// against an independent reference at every one of the eight masks rather than
// requiring two implementations to agree about which mask is best.
func encodeWithMask(data []byte, ecc ECC, version, mask int) (*Symbol, error) {
	if mask < 0 || mask > 7 {
		return nil, fmt.Errorf("qr: mask %d out of range 0-7", mask)
	}
	s, err := EncodeVersion(data, ecc, version)
	if err != nil {
		return nil, err
	}
	// EncodeVersion left its own chosen mask applied; XOR is its own inverse.
	s.applyMask(s.Mask)
	s.applyMask(mask)
	s.drawFormatInfo(mask)
	s.Mask = mask
	return s, nil
}
