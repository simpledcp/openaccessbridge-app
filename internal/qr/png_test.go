package qr

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

// A QR pressed against the edge of its image is materially harder for a phone to
// acquire, and the failure looks like "the code doesn't work" rather than like a
// margin problem. The quiet zone is therefore not optional, and this test fails
// if it is ever quietly dropped to make an image smaller.
func TestTheRenderedImageKeepsItsQuietZone(t *testing.T) {
	// The literal is the point. Written as QuietZone, this test measured the
	// margin against the very constant it was meant to police: setting
	// QuietZone to 0 changed both sides of every comparison and the test passed
	// with the behaviour removed. Four is what ISO/IEC 18004 requires, so four is
	// what is written here.
	const specQuietZone = 4
	if QuietZone < specQuietZone {
		t.Fatalf("QuietZone is %d modules; the standard requires at least %d", QuietZone, specQuietZone)
	}
	sym, err := Encode([]byte("http://192.168.44.1:8080/accessibility?t=v1.abc.def"), DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 4
	var buf bytes.Buffer
	if err := sym.PNG(&buf, scale); err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(&buf)
	if err != nil {
		t.Fatalf("the appliance produced a PNG nothing can decode: %v", err)
	}
	want := (sym.Size + 2*specQuietZone) * scale
	if b := img.Bounds(); b.Dx() != want || b.Dy() != want {
		t.Fatalf("image is %dx%d, want %dx%d (symbol %d modules + %d quiet on each side, x%d)",
			b.Dx(), b.Dy(), want, want, sym.Size, QuietZone, scale)
	}
	// Every pixel in the margin must be light, on all four sides.
	for i := 0; i < specQuietZone*scale; i++ {
		for j := 0; j < want; j++ {
			for _, p := range []image.Point{{X: i, Y: j}, {X: j, Y: i},
				{X: want - 1 - i, Y: j}, {X: j, Y: want - 1 - i}} {
				r, g, b, _ := img.At(p.X, p.Y).RGBA()
				if r == 0 && g == 0 && b == 0 {
					t.Fatalf("a dark pixel at (%d,%d) is inside the quiet zone", p.X, p.Y)
				}
			}
		}
	}
}

// The symbol's first module must land exactly where the quiet zone ends: an
// off-by-one here shifts every module and produces an image that is a QR code
// of something else entirely.
func TestTheSymbolStartsWhereTheQuietZoneEnds(t *testing.T) {
	sym, err := Encode([]byte("CB"), DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	const scale = 3
	var buf bytes.Buffer
	if err := sym.PNG(&buf, scale); err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(&buf)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < sym.Size; y++ {
		for x := 0; x < sym.Size; x++ {
			px := (x+QuietZone)*scale + scale/2
			py := (y+QuietZone)*scale + scale/2
			r, _, _, _ := img.At(px, py).RGBA()
			dark := r == 0
			if dark != sym.Dark(x, y) {
				t.Fatalf("module (%d,%d) is %v in the symbol and %v in the image",
					x, y, sym.Dark(x, y), dark)
			}
		}
	}
}

// A scale outside the supported range is refused rather than clamped: a caller
// asking for a size the appliance will not render should be told, not handed a
// different image than it asked for.
func TestAnImpossibleScaleIsRefused(t *testing.T) {
	sym, err := Encode([]byte("CB"), DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	for _, scale := range []int{0, -1, MaxScale + 1} {
		if err := sym.PNG(&bytes.Buffer{}, scale); err == nil {
			t.Errorf("scale %d was accepted", scale)
		}
	}
}

// The print guidance must grow with the symbol. A longer join URL means a bigger
// version, more modules, and a card that has to be physically larger to stay
// scannable — the relationship a booth cannot see on a screen.
func TestPrintGuidanceGrowsWithTheSymbol(t *testing.T) {
	small, err := Encode([]byte("CB"), DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	big, err := Encode(fill(700), DefaultECC)
	if err != nil {
		t.Fatal(err)
	}
	if big.Version <= small.Version {
		t.Fatalf("the long payload did not need a larger version (%d vs %d)", big.Version, small.Version)
	}
	if big.RecommendedPrintWidthMM() <= small.RecommendedPrintWidthMM() {
		t.Fatalf("a %d-module symbol is advised at %d mm and a %d-module one at %d mm",
			big.Modules(), big.RecommendedPrintWidthMM(),
			small.Modules(), small.RecommendedPrintWidthMM())
	}
}
