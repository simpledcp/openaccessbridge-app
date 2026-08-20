package acsp

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// announceTo drives a fresh machine to ANNOUNCED with the given announcement
// and returns the identity outcome plus any warnings it raised.
func announceTo(t *testing.T, opts Options, serverID, screenName, fingerprint string) (IdentityState, []model.Warning) {
	t.Helper()
	var warnings []model.Warning
	opts.OnWarning = func(w model.Warning) { warnings = append(warnings, w) }
	m := New(opts)
	m.Connected()
	m.Handle(Request{Type: ReqAnnounce, ServerID: serverID, ScreenName: screenName, Fingerprint: fingerprint})
	return m.Identity(), warnings
}

func hasCode(ws []model.Warning, code string) *model.Warning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

// §8.5: "Pin the expected server identity per auditorium profile (expected IP,
// ACSP server fingerprint, AND SCREEN NAME); warn loudly if the connected server
// does not match." Only the fingerprint was ever compared — the screen name was
// received and discarded, so the pin §10.5 says is most often the ONLY one
// available did nothing.
func TestScreenNameMismatchIsCaught(t *testing.T) {
	state, ws := announceTo(t, Options{ExpectedScreenName: "Auditorium 3"},
		"server-1", "Auditorium 7", "")

	if state != IdentityMismatch {
		t.Fatalf("identity = %q, want mismatch — the appliance is cabled to the wrong room", state)
	}
	w := hasCode(ws, model.WarnServerIdentityMismatch)
	if w == nil {
		t.Fatal("a wrong-room mismatch raised no warning")
	}
	if w.Severity != model.SeverityCritical {
		t.Errorf("severity = %q, want critical", w.Severity)
	}
	if !strings.Contains(w.Message, "screen name") {
		t.Errorf("message should name WHICH pin failed, got %q", w.Message)
	}
}

// The screen name is operator-typed on one side and server-reported on the
// other, so casing and stray whitespace must not read as a wrong room.
func TestScreenNameMatchIsWhitespaceAndCaseInsensitive(t *testing.T) {
	for _, announced := range []string{"Auditorium 3", "auditorium 3", "  AUDITORIUM 3  "} {
		state, ws := announceTo(t, Options{ExpectedScreenName: "Auditorium 3"}, "server-1", announced, "")
		if state != IdentityMatch {
			t.Errorf("announced %q: identity = %q, want match", announced, state)
		}
		if hasCode(ws, model.WarnServerIdentityMismatch) != nil {
			t.Errorf("announced %q raised a wrong-room warning", announced)
		}
	}
	// But nothing cleverer: a genuinely different room must still be caught.
	if state, _ := announceTo(t, Options{ExpectedScreenName: "Auditorium 3"}, "server-1", "Screen 3", ""); state != IdentityMismatch {
		t.Errorf("\"Screen 3\" vs \"Auditorium 3\" = %q; normalizing that away would silence a real re-cabling", state)
	}
}

// A pin that the server never answers means the guard DID NOT RUN. Reporting
// that as a match is the same defect class as reporting a firewall active
// without reading the ruleset (§9.3): a green light for a check nobody made.
func TestPinnedButUnannouncedIdentityIsUnverifiedNotMatched(t *testing.T) {
	state, ws := announceTo(t, Options{ExpectedFingerprint: "abc123"}, "server-1", "Auditorium 3", "")
	if state == IdentityMatch {
		t.Fatal("a pinned fingerprint the server never announced was reported as a MATCH")
	}
	if state != IdentityUnverified {
		t.Fatalf("identity = %q, want unverified", state)
	}
	if state.Verified() {
		t.Error("IdentityUnverified.Verified() must be false")
	}
	w := hasCode(ws, model.WarnServerIdentityMismatch)
	if w == nil {
		t.Fatal("an unperformable wrong-room check must still surface (guardrail #7)")
	}
	if w.Severity == model.SeverityCritical {
		t.Error("could-not-check should not be as loud as a confirmed mismatch")
	}
}

// With nothing pinned there is nothing to check. That is a setup gap, reported
// honestly — not a fault, and not a match either.
func TestUnpinnedIdentityIsNeitherMatchNorFault(t *testing.T) {
	state, ws := announceTo(t, Options{}, "server-1", "Auditorium 3", "fp")
	if state != IdentityUnpinned {
		t.Fatalf("identity = %q, want unpinned", state)
	}
	if state.Verified() {
		t.Error("an unpinned identity must not report as verified")
	}
	if len(ws) != 0 {
		t.Errorf("an unpinned auditorium should not raise a wrong-room warning: %+v", ws)
	}
}

// Both pins present and both answered correctly is the only verified state.
func TestBothPinsMatchingIsVerified(t *testing.T) {
	state, ws := announceTo(t,
		Options{ExpectedFingerprint: "abc123", ExpectedScreenName: "Auditorium 3"},
		"server-1", "Auditorium 3", "abc123")
	if state != IdentityMatch || !state.Verified() {
		t.Fatalf("identity = %q, want a verified match", state)
	}
	if len(ws) != 0 {
		t.Errorf("a clean identity check should raise nothing: %+v", ws)
	}
}

// A fingerprint that matches must not excuse a screen name that does not: the
// common multiplex failure is one appliance cabled to the wrong room's server,
// where whichever pin is weakest is the one that catches it.
func TestOneGoodPinDoesNotExcuseABadOne(t *testing.T) {
	state, ws := announceTo(t,
		Options{ExpectedFingerprint: "abc123", ExpectedScreenName: "Auditorium 3"},
		"server-1", "Auditorium 7", "abc123")
	if state != IdentityMismatch {
		t.Fatalf("identity = %q, want mismatch", state)
	}
	if w := hasCode(ws, model.WarnServerIdentityMismatch); w == nil || !strings.Contains(w.Message, "screen name") {
		t.Errorf("the warning should name the screen name as the failing pin, got %+v", w)
	}
}

// The identity check must never log the operator's pinned values or the
// server's announced ones (§9.4: no operator free text, no site identifiers).
func TestIdentityCheckLogsNoIdentifiers(t *testing.T) {
	var buf strings.Builder
	opts := Options{ExpectedScreenName: "Auditorium 3", Log: testLogger(&buf)}
	announceTo(t, opts, "SECRET-SERVER-ID", "SECRET-SCREEN", "SECRET-FP")

	for _, secret := range []string{"SECRET-SERVER-ID", "SECRET-SCREEN", "SECRET-FP", "Auditorium 3"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("identity check logged %q:\n%s", secret, buf.String())
		}
	}
}

// testLogger builds a JSON logger writing into buf, so a test can assert what
// does and does not reach the log.
func testLogger(buf *strings.Builder) *logging.Logger {
	return logging.New(logging.Options{Component: "core", Writer: buf, JSON: true})
}
