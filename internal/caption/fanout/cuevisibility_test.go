package fanout

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// §9.4 names `caption.cue.visible_no_text` as the sanctioned trade against the
// no-content rule: "records that a cue was shown and for how long, not its
// words — enough to debug a 'captions look wrong' report without storing
// content". The tracker implemented it and nothing called it, so the only way to
// investigate such a report was to capture the text itself.
func TestACueInAndOutEmitsTheContentFreeEvent(t *testing.T) {
	var logs strings.Builder
	tracker := logging.NewCueVisibilityTracker(
		logging.New(logging.Options{Component: "test", JSON: true, Writer: &logs}),
		logging.CueVisibilityOptions{})

	h := NewHub(Options{OnCueVisibility: tracker.Observe})
	const secret = "I told you never to come back here"
	cue := model.Cue{ResourceID: "res-1", StartMS: 1000, Text: secret}

	h.PublishCue("en", true, cue)
	h.PublishCue("en", false, cue)

	out := logs.String()
	if !strings.Contains(out, "caption.cue.visible_no_text") {
		t.Fatalf("no cue-visibility event was emitted:\n%s", out)
	}
	// THE POINT OF THE EVENT: the words never travel.
	if strings.Contains(out, secret) || strings.Contains(out, "never to come back") {
		t.Fatalf("the caption text reached the log:\n%s", out)
	}
	// Nor does the raw cue id — §9.4 wants opaque/hashed identifiers.
	if strings.Contains(out, "res-1@1000") {
		t.Fatalf("the raw cue id reached the log unhashed:\n%s", out)
	}
	if !strings.Contains(out, "en") {
		t.Fatalf("the language is not recorded, so the event cannot answer which track:\n%s", out)
	}

	// The hashed value must be the cue's IDENTIFIER, not its text.
	//
	// This looks pedantic and is not. The tracker hashes whatever it is handed,
	// so passing the caption line instead of the id leaks nothing obvious and
	// every other assertion here still passes — but a caption line is
	// low-entropy, and a hash of one is confirmable against a candidate script by
	// anyone holding the log. §9.4 asks for opaque identifiers precisely so the
	// log cannot answer "was this line shown".
	wantID := logging.HashID("cue", "res-1@1000")
	if !strings.Contains(out, wantID) {
		t.Fatalf("the event does not carry the hashed cue id (%s):\n%s", wantID, out)
	}
	if textHash := logging.HashID("cue", secret); strings.Contains(out, textHash) {
		t.Fatalf("the event carries a hash of the caption TEXT: a short line is confirmable " +
			"against a candidate script by anyone holding the log")
	}
}

// A hub built without the hook logs nothing: the fan-out never decides for
// itself what to record.
func TestAHubWithNoHookEmitsNothing(t *testing.T) {
	h := NewHub(Options{})
	h.PublishCue("en", true, model.Cue{ResourceID: "res-1", StartMS: 1, Text: "x"})
	h.PublishCue("en", false, model.Cue{ResourceID: "res-1", StartMS: 1})
	// No panic and no logger to write to: the assertion is that this is a
	// supported configuration rather than a nil dereference.
}
