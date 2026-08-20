package caption

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
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
