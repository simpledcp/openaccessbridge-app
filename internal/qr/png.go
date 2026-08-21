package qr

// Rendering a symbol to a PNG a phone can actually read.
//
// The two things that decide whether a scan succeeds in a cinema are not in the
// encoder: the quiet zone and the module size. Both are handled here, and both
// are stated rather than left to a caller's guess.

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
)

// QuietZone is the light margin around a symbol, in modules.
//
// Four is the standard's requirement, not a stylistic choice: a scanner locates
// the symbol by the contrast between the finder patterns and the space around
// them, and a QR pasted flush against dark artwork — or against the edge of a
// card — is materially harder to acquire. It is not a caller's option here
// because the one thing worse than a QR that is hard to scan is one that is hard
// to scan for a reason nobody can see.
const QuietZone = 4

// MaxScale bounds the module size so a malformed request cannot ask the
// appliance to allocate an enormous image. At the top of the range a version 40
// symbol (177 modules plus 4 of quiet zone on each side, so 185) is 5920 px
// square — already far past any screen or printer a booth will use.
const MaxScale = 32

// Modules returns the width of the rendered symbol in modules, quiet zone
// included. It is what a caller needs to work out a physical size.
func (s *Symbol) Modules() int { return s.Size + 2*QuietZone }

// PNG writes the symbol as a PNG, scale pixels per module, with the quiet zone
// included.
func (s *Symbol) PNG(w io.Writer, scale int) error {
	if scale < 1 || scale > MaxScale {
		return fmt.Errorf("qr: scale %d out of range 1-%d", scale, MaxScale)
	}
	side := s.Modules() * scale
	// A two-colour paletted image: a QR is one bit per module, and encoding it
	// as anything richer makes a file several times larger for no gain in
	// contrast — and contrast is the whole point.
	img := image.NewPaletted(image.Rect(0, 0, side, side), color.Palette{
		color.White, color.Black,
	})
	for y := 0; y < s.Size; y++ {
		for x := 0; x < s.Size; x++ {
			if !s.Dark(x, y) {
				continue
			}
			px, py := (x+QuietZone)*scale, (y+QuietZone)*scale
			for dy := 0; dy < scale; dy++ {
				row := img.Pix[(py+dy)*img.Stride+px:]
				for dx := 0; dx < scale; dx++ {
					row[dx] = 1
				}
			}
		}
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	return enc.Encode(w, img)
}

// RecommendedPrintWidthMM is the smallest width a printed symbol should be, for
// a phone held at roughly arm's length in a cinema lobby.
//
// The rule of thumb scanners are built around is that a module needs to be about
// 0.5 mm before a typical phone camera struggles, and that comfortable reading
// wants nearer 0.8 mm — more, not less, in the low light a foyer or an
// auditorium door actually has. This returns the width at 0.8 mm per module,
// rounded up to a whole millimetre.
//
// It exists so staff guidance is a NUMBER derived from the symbol actually being
// printed rather than a guess: a longer join URL means a larger version, more
// modules, and a card that has to be physically bigger to stay scannable. That
// relationship is invisible on a screen, which is exactly where a booth would
// get it wrong.
func (s *Symbol) RecommendedPrintWidthMM() int {
	const mmPerModule = 0.8
	w := float64(s.Modules()) * mmPerModule
	if w > float64(int(w)) {
		return int(w) + 1
	}
	return int(w)
}
