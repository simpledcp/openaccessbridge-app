package qr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestWritePNGCorpus renders the fixture corpus as real PNGs, exactly as the
// admin surface serves them, into the directory named by QR_PNG_OUT.
//
// It is the first half of `make qr-verify`. The second half is
// testdata/verify_png.py, which feeds those files to zxing-cpp and requires the
// exact payload back. The two halves are separate because CI has no Python and
// must not need any: the committed fixtures already pin every module, and this
// target exists to close the last gap they cannot — that the IMAGE, at the
// scales the appliance actually serves, is one a scanner reads.
//
// Without QR_PNG_OUT this test does nothing and says so, rather than silently
// passing as though it had verified something.
func TestWritePNGCorpus(t *testing.T) {
	outDir := os.Getenv("QR_PNG_OUT")
	if outDir == "" {
		t.Skip("QR_PNG_OUT is not set: nothing rendered, nothing verified (see `make qr-verify`)")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		t.Fatal(err)
	}
	vf := loadVectors(t)
	manifest := map[string]string{}
	// The scales a booth actually gets: the admin page's inline image, a larger
	// screen rendering, and a print-resolution one.
	for _, scale := range []int{2, 4, 8} {
		for _, v := range vf.Cases {
			sym, err := Encode([]byte(v.Data), parseECC(t, v.ECC))
			if err != nil {
				t.Fatalf("%s: %v", v.Name, err)
			}
			name := v.Name + "_s" + itoa(scale) + ".png"
			f, err := os.Create(filepath.Join(outDir, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := sym.PNG(f, scale); err != nil {
				_ = f.Close()
				t.Fatalf("%s: %v", v.Name, err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			manifest[name] = v.Data
		}
	}
	m, err := os.Create(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := json.NewEncoder(m).Encode(manifest); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d PNGs to %s", len(manifest), outDir)
}
