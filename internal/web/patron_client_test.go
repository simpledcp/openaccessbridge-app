package web

import (
	"os/exec"
	"testing"
)

// TestPatronClient runs the browser-side behaviour tests for assets/patron.js.
//
// The patron page carries real logic — transport fallback, the show-end state, the
// language-substitution notice, the audio picker state machine and the §6.7 buffer
// control — and that logic is not reachable from a Go test. It is driven instead by
// a small DOM/transport stand-in under testdata/, in the same spirit as the
// GStreamer smoke test: it runs for real when its dependency is present and SKIPS
// honestly when it is not, rather than pretending to have exercised it.
//
// The stand-in simulates the browser API surface only. Real phone behaviour
// (autoplay policy, mDNS ICE, Wake Lock, captive portals) stays under BLOCKER B2
// and is asserted nowhere.
func TestPatronClient(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed: the patron client's browser-side logic is not exercised on this host")
	}
	out, err := exec.Command(node, "testdata/patron_client_test.js").CombinedOutput()
	t.Logf("patron client cases:\n%s", out)
	if err != nil {
		t.Fatalf("patron client behaviour tests failed: %v", err)
	}
}
