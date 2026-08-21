package acsp

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// §10.3 declares last_timeline_anchor_monotonic_ns and NOTHING ever assigned it,
// so the one diagnostic that says when the timeline last got a fix from the
// server read 0 on every appliance. Zero is also the value for "never anchored",
// which left the field unable to tell a running show from a dead one.
func TestTheAnchorTimestampIsPopulatedOnceTheTimelineMoves(t *testing.T) {
	st := playingMachine(t).State()
	if st.LastTimelineAnchorMonotonicNS == 0 {
		t.Fatal("the timeline anchored and the diagnostic still reads 0 (never anchored)")
	}
}

// A machine that has never anchored reports 0, so the field distinguishes the
// two states rather than always carrying a number.
//
// It also uses a machine with NO controller, which is a supported configuration
// the machine guards for elsewhere and State() did not: this panicked before,
// latently, because every other caller happened to supply one.
func TestAMachineThatNeverAnchoredReportsZero(t *testing.T) {
	m := New(Options{})
	if got := m.State().LastTimelineAnchorMonotonicNS; got != 0 {
		t.Fatalf("an un-anchored machine reports anchor %d", got)
	}
}

// §9.4's minimum event set names caption.timeline.anchor_updated, and nothing
// emitted it — so after a "captions are drifting" report there was no record of
// whether the server kept re-anchoring, or by how much the local prediction was
// out each time. §5.5's whole design is re-anchoring; this was its missing half.
func TestReAnchoringEmitsTheDiagnosticEvent(t *testing.T) {
	var logs strings.Builder
	playingMachineLogging(t, &logs)

	out := logs.String()
	if !strings.Contains(out, "caption.timeline.anchor_updated") {
		t.Fatalf("re-anchoring emitted no event:\n%s", out)
	}
	if !strings.Contains(out, "timeline_ms") || !strings.Contains(out, "drift_ms") {
		t.Fatalf("the event carries neither the position nor the drift:\n%s", out)
	}
	// §9.4: a timeline position says where the film is, never what anyone said.
	// Nothing content-bearing may ride along.
	if strings.Contains(out, "text") {
		t.Fatalf("something text-shaped reached the anchor event:\n%s", out)
	}
}

// playingMachine drives a machine to PLAYING with a moving timeline.
func playingMachine(t *testing.T) *Machine {
	t.Helper()
	var logs strings.Builder
	return playingMachineLogging(t, &logs)
}

func playingMachineLogging(t *testing.T, logs *strings.Builder) *Machine {
	t.Helper()
	// A real controller, as production has: a machine without one cannot process
	// a timeline at all, which is a precondition rather than something to guard.
	hub := fanout.NewHub(fanout.Options{})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	t.Cleanup(ctrl.Close)
	m := New(Options{
		Controller: ctrl,
		Log:        logging.New(logging.Options{Component: "test", JSON: true, Writer: logs}),
	})
	m.Connected()
	m.Handle(Request{Type: ReqUpdateTimeline, TimelineMS: 1000, PlayState: PlayPlaying})
	m.Handle(Request{Type: ReqUpdateTimeline, TimelineMS: 2000, PlayState: PlayPlaying})
	if m.State().State != model.StatePlaying {
		t.Fatalf("the machine is %q, not playing", m.State().State)
	}
	return m
}
