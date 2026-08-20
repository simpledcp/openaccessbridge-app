package fanout

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// TestCueVisibilityIsUnansweredWithNoLanguageLoaded: §8.2's "Current cue:
// visible / none" row. With no caption language served there is no cue to be
// visible OR absent, and a dashboard that printed "none" would be reporting a
// measurement it never made.
func TestCueVisibilityIsUnansweredWithNoLanguageLoaded(t *testing.T) {
	h := NewHub(Options{})
	if visible, known := h.CueVisible(); visible || known {
		t.Errorf("with no language loaded the question is unanswerable, got visible=%v known=%v", visible, known)
	}
}

// TestCueVisibilityFollowsThePublishedCue: once a language is served the row is
// a real observation of what patrons are being sent.
func TestCueVisibilityFollowsThePublishedCue(t *testing.T) {
	h := NewHub(Options{})
	h.SetTrackKind("en", model.TrackClosedCaption)

	if visible, known := h.CueVisible(); !known || visible {
		t.Errorf("a loaded language with no cue is a measured 'none', got visible=%v known=%v", visible, known)
	}

	h.PublishCue("en", true, model.Cue{ResourceID: "res-en", StartMS: 1000, Text: "Welcome to CaptionBridge."})
	if visible, known := h.CueVisible(); !known || !visible {
		t.Errorf("a published cue must read as visible, got visible=%v known=%v", visible, known)
	}

	h.PublishCue("en", false, model.Cue{})
	if visible, known := h.CueVisible(); !known || visible {
		t.Errorf("a cleared cue must read as none, got visible=%v known=%v", visible, known)
	}
}

// TestCueVisibilityAnswersAcrossLanguages: any served language showing a cue
// makes the row "visible" — the booth asks "is anything on screen", not "is the
// first language on screen".
func TestCueVisibilityAnswersAcrossLanguages(t *testing.T) {
	h := NewHub(Options{})
	h.SetTrackKind("en", model.TrackClosedCaption)
	h.SetTrackKind("es-419", model.TrackClosedSubtitle)
	h.PublishCue("es-419", true, model.Cue{ResourceID: "res-es", StartMS: 500, Text: "Bienvenido."})
	if visible, known := h.CueVisible(); !known || !visible {
		t.Errorf("a cue in any served language counts, got visible=%v known=%v", visible, known)
	}
}

// TestCueVisibilityExposesNoText: §9.4's caption.cue.visible_no_text principle —
// a cue's OCCURRENCE may reach a diagnostic surface, its words never may. The
// visibility accessor must therefore hand back nothing but booleans, and no
// exported hub method may offer the current text to an admin caller.
func TestCueVisibilityExposesNoText(t *testing.T) {
	h := NewHub(Options{})
	h.SetTrackKind("en", model.TrackClosedCaption)
	const secret = "this line is caption content"
	h.PublishCue("en", true, model.Cue{ResourceID: "res-en", StartMS: 0, Text: secret})

	visible, known := h.CueVisible()
	if !visible || !known {
		t.Fatalf("expected a visible cue, got visible=%v known=%v", visible, known)
	}
	// The only route to the text is a subscriber, i.e. the patron path.
	sub, err := h.Subscribe("en")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer h.Unsubscribe(sub)
	if !strings.Contains(sub.Peek().Text, secret) {
		t.Fatalf("the patron path still carries the cue text")
	}
}
