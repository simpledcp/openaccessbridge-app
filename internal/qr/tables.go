package qr

// The fixed tables of ISO/IEC 18004, and the Reed-Solomon arithmetic over
// GF(256) that the error-correction codewords are computed in.
//
// A transcription slip in any of these produces a symbol that is malformed for
// exactly one version/level combination and perfect for every other — the kind
// of defect that survives casual testing forever. So the tests do not spot-check
// them: testdata/exhaustive.json pins ALL 160 version x level combinations
// against an independent implementation, including each one's byte capacity and
// the SHA-256 of the full module matrix at that capacity.

// ecCodewordsPerBlockTable[ecc][version] is the number of error-correction
// codewords in each block. Index 0 of the version axis is unused so the version
// number indexes directly.
var ecCodewordsPerBlockTable = [4][41]int{
	// L
	{0, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	// M
	{0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28},
	// Q
	{0, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
	// H
	{0, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30},
}

// numBlocksTable[ecc][version] is the number of error-correction blocks the data
// codewords are split across.
var numBlocksTable = [4][41]int{
	// L
	{0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25},
	// M
	{0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49},
	// Q
	{0, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68},
	// H
	{0, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81},
}

// alignmentCentres[version] lists the row/column coordinates whose crossings
// carry an alignment pattern (minus the three occupied by finders). Version 1
// has none.
var alignmentCentres = [41][]int{
	{}, {}, // 0 unused, 1 has none
	{6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34},
	{6, 22, 38}, {6, 24, 42}, {6, 26, 46}, {6, 28, 50}, {6, 30, 54}, {6, 32, 58}, {6, 34, 62},
	{6, 26, 46, 66}, {6, 26, 48, 70}, {6, 26, 50, 74}, {6, 30, 54, 78}, {6, 30, 56, 82}, {6, 30, 58, 86}, {6, 34, 62, 90},
	{6, 28, 50, 72, 94}, {6, 26, 50, 74, 98}, {6, 30, 54, 78, 102}, {6, 28, 54, 80, 106}, {6, 32, 58, 84, 110}, {6, 30, 58, 86, 114}, {6, 34, 62, 90, 118},
	{6, 26, 50, 74, 98, 122}, {6, 30, 54, 78, 102, 126}, {6, 26, 52, 78, 104, 130}, {6, 30, 56, 82, 108, 134}, {6, 34, 60, 86, 112, 138}, {6, 30, 58, 86, 114, 142}, {6, 34, 62, 90, 118, 146},
	{6, 30, 54, 78, 102, 126, 150}, {6, 24, 50, 76, 102, 128, 154}, {6, 28, 54, 80, 106, 132, 158}, {6, 32, 58, 84, 110, 136, 162}, {6, 26, 54, 82, 110, 138, 166}, {6, 30, 58, 86, 114, 142, 170},
}

func alignmentCoords(version int) []int { return alignmentCentres[version] }

func ecCodewordsPerBlock(version int, ecc ECC) int {
	return ecCodewordsPerBlockTable[ecc][version]
}

func numBlocks(version int, ecc ECC) int { return numBlocksTable[ecc][version] }

// totalCodewords is the number of 8-bit codewords a version holds, derived from
// its geometry rather than tabulated: total modules, minus the function
// patterns, minus the remainder bits that do not make up a whole codeword.
//
// Deriving it removes a 40-entry table that could be mistyped, and it is checked
// against an independent implementation for all 40 versions anyway.
func totalCodewords(version int) int {
	return rawDataModules(version) / 8
}

// rawDataModules counts the modules available to data and error correction:
// everything except finders, separators, timing, alignment, format and version
// information.
func rawDataModules(version int) int {
	size := version*4 + 17
	modules := size * size
	modules -= 3 * 8 * 8       // three finder patterns with their separators
	modules -= 2 * (size - 16) // the two timing patterns, outside the finders
	modules -= 31              // format information plus the dark module
	if version >= 7 {
		modules -= 2 * 18 // the two version-information blocks
	}
	if version >= 2 {
		n := len(alignmentCentres[version])
		// n^2 alignment patterns of 25 modules, less the three that would sit on
		// finders, and less the modules each one overlaps with a timing pattern.
		modules -= (n*n - 3) * 25
		modules += (n - 2) * 5 * 2 // the ones on the timing rows/columns overlap 5 each
	}
	return modules
}

// dataCodewords is how many of a version's codewords carry payload rather than
// error correction.
func dataCodewords(version int, ecc ECC) int {
	return totalCodewords(version) - ecCodewordsPerBlock(version, ecc)*numBlocks(version, ecc)
}

// byteCapacity is the longest byte-mode payload a version/level holds, after the
// four-bit mode indicator and the character-count indicator.
func byteCapacity(version int, ecc ECC) int {
	bits := dataCodewords(version, ecc)*8 - 4 - byteCountBits(version)
	if bits < 0 {
		return 0
	}
	return bits / 8
}

func smallestVersion(dataLen int, ecc ECC) (int, error) {
	for v := 1; v <= 40; v++ {
		if byteCapacity(v, ecc) >= dataLen {
			return v, nil
		}
	}
	return 0, ErrTooLong
}

// --- Reed-Solomon over GF(256), primitive polynomial x^8+x^4+x^3+x^2+1 ---

var (
	gfExp [512]byte
	gfLog [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

// rsGenerator builds the degree-n generator polynomial, coefficients
// high-order first.
func rsGenerator(degree int) []byte {
	g := []byte{1}
	for i := 0; i < degree; i++ {
		// Multiply g by (x - alpha^i).
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= gfMul(c, gfExp[i])
		}
		g = next
	}
	return g
}

// rsRemainder is the error-correction codeword block for one data block.
func rsRemainder(data, gen []byte) []byte {
	rem := make([]byte, len(gen)-1)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[len(rem)-1] = 0
		for i, g := range gen[1:] {
			rem[i] ^= gfMul(g, factor)
		}
	}
	return rem
}
