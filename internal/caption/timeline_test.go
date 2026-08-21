package caption

import (
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// TestTimelineRowTracksTheTransportState: §8.2's "Timeline: playing / paused /
// stopped" row. It is read from the REFERENCE engine, which is driven by the
// ACSP layer's Play/Pause/Stop/Resume, so it is a measurement of what the server
// told this appliance rather than an inference from the handshake state.
func TestTimelineRowTracksTheTransportState(t *testing.T) {
	c := NewController(Options{Hub: fanout.NewHub(fanout.Options{})})
	defer c.Close()

	if got := c.Timeline(); got != model.TimelineNone {
		t.Errorf("a controller nobody has driven has no timeline, got %q", got)
	}
	c.Play(0)
	if got := c.Timeline(); got != model.TimelinePlaying {
		t.Errorf("after Play the timeline is playing, got %q", got)
	}
	c.Pause()
	if got := c.Timeline(); got != model.TimelinePaused {
		t.Errorf("after Pause the timeline is paused, got %q", got)
	}
	c.Resume()
	if got := c.Timeline(); got != model.TimelinePlaying {
		t.Errorf("after Resume the timeline is playing, got %q", got)
	}
	c.Stop()
	if got := c.Timeline(); got != model.TimelineStopped {
		t.Errorf("after Stop the timeline is stopped, got %q", got)
	}
}

// TestExpiredLeaseReportsNoTimeline: once the lease has genuinely expired the
// server is no longer telling this appliance a transport state, so echoing the
// last one would present a stale fact as current (guardrail #7). The row says
// "no timeline" instead — and, in particular, does not say "playing".
func TestExpiredLeaseReportsNoTimeline(t *testing.T) {
	c := NewController(Options{Hub: fanout.NewHub(fanout.Options{})})
	defer c.Close()
	c.Play(5000)
	c.LeaseExpired()
	if got := c.Timeline(); got != model.TimelineNone {
		t.Errorf("an expired lease has no current transport state, got %q", got)
	}
}

// TestCompositionChangeDropsTheTimeline: a new composition mid-lease discards
// the prediction until the next anchor lands, so the row must not keep claiming
// the outgoing show's transport state.
func TestCompositionChangeDropsTheTimeline(t *testing.T) {
	c := NewController(Options{Hub: fanout.NewHub(fanout.Options{})})
	defer c.Close()
	c.Play(1000)
	c.CompositionChanged()
	if got := c.Timeline(); got != model.TimelineNone {
		t.Errorf("a composition change leaves no anchored timeline, got %q", got)
	}
}

// TestServerConnectedIsFalseForFaultedLinks: §8.2's "Server connected: yes/no".
// DISCONNECTED and ERROR both answer no; an unset state answers no as well,
// because nothing has ever connected.
func TestServerConnectedIsFalseForFaultedLinks(t *testing.T) {
	for _, st := range []model.ACSPState{model.StateDisconnected, model.StateError, ""} {
		if (model.CaptionLinkState{State: st}).ServerConnected() {
			t.Errorf("state %q must not read as connected", st)
		}
	}
	for _, st := range []model.ACSPState{
		model.StateConnected, model.StateAnnounced, model.StateLeased,
		model.StatePlaying, model.StatePaused, model.StateLeaseExpired,
	} {
		if !(model.CaptionLinkState{State: st}).ServerConnected() {
			t.Errorf("state %q holds a link and must read as connected", st)
		}
	}
}

// TestDriftIsUnmeasuredUntilAnEngineExists: MaxDriftMS ranges over the
// per-language engines, so a controller with none returns 0 by having nothing to
// range over. §8.2's drift row is an int64 with no "unset", so a dashboard that
// trusted the figure alone would print "0 ms" — perfect sync — for a caption
// path that is not running at all.
func TestDriftIsUnmeasuredUntilAnEngineExists(t *testing.T) {
	c := NewController(Options{Hub: fanout.NewHub(fanout.Options{})})
	defer c.Close()

	if c.MaxDriftMS() != 0 {
		t.Fatalf("precondition: an empty controller reports 0 drift")
	}
	if c.DriftMeasured() {
		t.Errorf("with no timing engine loaded there is nothing to measure drift against")
	}
	// Driving the transport does not conjure an engine either: Play on an empty
	// controller still has no language to compare against.
	c.Play(0)
	if c.DriftMeasured() {
		t.Errorf("a transport state is not a drift measurement")
	}

	c.LoadReel(map[string][]model.Cue{"en": {{StartMS: 0, EndMS: 1000, Text: "x"}}})
	if !c.DriftMeasured() {
		t.Errorf("once a language engine is loaded the drift figure is a measurement")
	}
}
