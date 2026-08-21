package qr

// The function patterns, format/version information, and the penalty rules that
// choose a mask (ISO/IEC 18004 §6.3, §6.8, §6.9).
//
// Everything here is fixed by the standard; none of it is an OpenAccessBridge
// decision. It is separated from qr.go so the encoding pipeline reads as a
// pipeline and the geometry reads as geometry.

// drawFunctionPatterns lays down everything that is not data: the three finder
// patterns and their separators, the timing patterns, the alignment patterns,
// the always-dark module, and the reserved areas for format and version
// information.
func (s *Symbol) drawFunctionPatterns() {
	// Timing patterns: alternating modules along row 6 and column 6, dark at
	// even coordinates.
	for i := 0; i < s.Size; i++ {
		s.setFunction(6, i, i%2 == 0)
		s.setFunction(i, 6, i%2 == 0)
	}

	// Finder patterns at three corners, with their separators. The missing
	// fourth corner is how a scanner works out the symbol's rotation.
	s.drawFinder(3, 3)
	s.drawFinder(s.Size-4, 3)
	s.drawFinder(3, s.Size-4)

	// Alignment patterns at every crossing of the version's coordinate list,
	// except where one would collide with a finder.
	coords := alignmentCoords(s.Version)
	for i, cx := range coords {
		for j, cy := range coords {
			last := len(coords) - 1
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			s.drawAlignment(cx, cy)
		}
	}

	// Reserve the format-information areas so data placement steps over them.
	// They are written after masking, because the format bits encode the mask.
	s.reserveFormatAreas()

	// The dark module: always dark, always at this coordinate, and it belongs to
	// no pattern at all.
	s.setFunction(8, s.Size-8, true)

	if s.Version >= 7 {
		s.drawVersionInfo()
	}
}

func (s *Symbol) drawFinder(cx, cy int) {
	// A 7x7 finder plus its one-module separator: dark where the Chebyshev
	// distance from the centre is 0, 1 (no: that is light), 2 or 3... expressed
	// directly, a ring at distance 2 is light and the rest to distance 3 is dark.
	for dy := -4; dy <= 4; dy++ {
		for dx := -4; dx <= 4; dx++ {
			x, y := cx+dx, cy+dy
			if x < 0 || y < 0 || x >= s.Size || y >= s.Size {
				continue
			}
			d := max(abs(dx), abs(dy))
			s.setFunction(x, y, d != 2 && d <= 3)
		}
	}
}

func (s *Symbol) drawAlignment(cx, cy int) {
	for dy := -2; dy <= 2; dy++ {
		for dx := -2; dx <= 2; dx++ {
			s.setFunction(cx+dx, cy+dy, max(abs(dx), abs(dy)) != 1)
		}
	}
}

// reserveFormatAreas marks the 31 modules the format information occupies (15
// bits, written twice) so drawCodewords skips them.
func (s *Symbol) reserveFormatAreas() {
	for i := 0; i <= 8; i++ {
		if i != 6 {
			s.setFunction(i, 8, false)
			s.setFunction(8, i, false)
		}
	}
	s.setFunction(8, 8, false)
	for i := 0; i < 8; i++ {
		s.setFunction(s.Size-1-i, 8, false)
	}
	for i := 0; i < 7; i++ {
		s.setFunction(8, s.Size-1-i, false)
	}
}

// drawFormatInfo writes the 15-bit format information — five data bits (ECC
// level and mask) protected by a BCH(15,5) code and XORed with a fixed mask so
// an all-zero format is never valid — into both of its locations.
func (s *Symbol) drawFormatInfo(mask int) {
	data := s.ECC.formatBits()<<3 | mask
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412

	// Copy 1: around the top-left finder.
	for i := 0; i <= 5; i++ {
		s.setFunction(8, i, bit(bits, i))
	}
	s.setFunction(8, 7, bit(bits, 6))
	s.setFunction(8, 8, bit(bits, 7))
	s.setFunction(7, 8, bit(bits, 8))
	for i := 9; i < 15; i++ {
		s.setFunction(14-i, 8, bit(bits, i))
	}

	// Copy 2: split between the other two finders, so a symbol with one damaged
	// corner is still readable.
	for i := 0; i < 8; i++ {
		s.setFunction(s.Size-1-i, 8, bit(bits, i))
	}
	for i := 8; i < 15; i++ {
		s.setFunction(8, s.Size-15+i, bit(bits, i))
	}
}

// drawVersionInfo writes the 18-bit version information (6 data bits plus a
// BCH(18,6) remainder) that versions 7 and above carry in two places. Nothing
// below version 7 has it, which is why a missing block still looks like a
// perfectly good QR on a small symbol and fails on a large one.
func (s *Symbol) drawVersionInfo() {
	rem := s.Version
	for i := 0; i < 12; i++ {
		rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
	}
	bits := s.Version<<12 | rem

	for i := 0; i < 18; i++ {
		b := bit(bits, i)
		a, c := s.Size-11+i%3, i/3
		s.setFunction(a, c, b)
		s.setFunction(c, a, b)
	}
}

func bit(v, i int) bool { return (v>>uint(i))&1 == 1 }

// rowRun and colRun read one line of the symbol for the penalty pass.
func (s *Symbol) rowRun(y int) []bool {
	out := make([]bool, s.Size)
	for x := 0; x < s.Size; x++ {
		out[x] = s.Dark(x, y)
	}
	return out
}

func (s *Symbol) colRun(x int) []bool {
	out := make([]bool, s.Size)
	for y := 0; y < s.Size; y++ {
		out[y] = s.Dark(x, y)
	}
	return out
}

// penalty scores a masked symbol under the four rules of §6.8.3.2. Lower is
// better. The rules penalise runs, blocks, finder-lookalike sequences, and an
// unbalanced dark/light ratio — none of which is a formal error. They exist
// because each one makes a scanner's job harder, so a badly masked symbol is a
// symbol some phones cannot read.
//
// Rules 1 and 3 are evaluated in one pass per line, over a rolling history of
// run lengths. A hand-rolled rule 3 written from the prose is where the first
// attempt at this file went wrong: it agreed with the reference implementations
// on nine of fourteen fixtures and picked a different mask on the other five,
// which is exactly the kind of "mostly right" that ships.
const (
	penaltyN1 = 3  // a run of five or more same-coloured modules
	penaltyN2 = 3  // a 2x2 block of one colour
	penaltyN3 = 40 // a finder-like 1:1:3:1:1 sequence with a light margin
	penaltyN4 = 10 // each 5% of dark/light imbalance past 50%
)

func (s *Symbol) penalty() int {
	score := 0

	// Rule 1: runs of five or more, in every row and column. A run of length L
	// costs L-2, which is N1 for five plus one for each module past five.
	for i := 0; i < s.Size; i++ {
		score += runPenalty(s.rowRun(i))
		score += runPenalty(s.colRun(i))
	}

	// Rule 2: 2x2 blocks of a single colour.
	for y := 0; y < s.Size-1; y++ {
		for x := 0; x < s.Size-1; x++ {
			c := s.Dark(x, y)
			if c == s.Dark(x+1, y) && c == s.Dark(x, y+1) && c == s.Dark(x+1, y+1) {
				score += penaltyN2
			}
		}
	}

	// Rule 3: finder-lookalikes.
	for i := 0; i < s.Size; i++ {
		score += penaltyN3 * countFinderLike(s.rowRun(i))
		score += penaltyN3 * countFinderLike(s.colRun(i))
	}

	// Rule 4: how far the dark proportion sits from half, in whole 5% steps.
	dark := 0
	for _, m := range s.modules {
		if m {
			dark++
		}
	}
	total := s.Size * s.Size
	score += abs(dark*20-total*10) / total * penaltyN4
	return score
}

func runPenalty(line []bool) int {
	score, run := 0, 1
	for i := 1; i < len(line); i++ {
		if line[i] == line[i-1] {
			run++
			continue
		}
		if run >= 5 {
			score += run - 2
		}
		run = 1
	}
	if run >= 5 {
		score += run - 2
	}
	return score
}

// countFinderLike counts the 1:1:3:1:1 dark/light sequences with a four-module
// light margin on one side, in one row or column:
//
//	pattern A: 1 0 1 1 1 0 1 0 0 0 0
//	pattern B: 0 0 0 0 1 0 1 1 1 0 1
//
// WHERE IMPLEMENTATIONS DIVERGE, because this is the one place in the format
// where careful people disagree. The standard does not say what lies beyond the
// symbol's edge, so some encoders treat the quiet zone as the light margin and
// count a pattern flush against the edge; others, including the reference this
// package's fixtures come from, only consider windows that fit entirely inside
// the symbol. Both produce valid, scannable symbols — the mask is recorded in
// the format information, so a scanner never has to agree with our reasoning,
// only read our answer — but the two choose different masks on some inputs.
//
// This implementation takes the in-bounds reading. What actually matters is
// stated in the tests: the choice is deterministic, and every symbol is verified
// decodable by an independent decoder.
func countFinderLike(line []bool) int {
	count := 0
	for i := 0; i+10 < len(line); i++ {
		w := line[i : i+11]
		if w[1] || !w[4] || w[5] || !w[6] || w[9] {
			continue
		}
		a := w[0] && w[2] && w[3] && !w[7] && !w[8] && !w[10]
		b := !w[0] && !w[2] && !w[3] && w[7] && w[8] && w[10]
		if a || b {
			count++
		}
	}
	return count
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
