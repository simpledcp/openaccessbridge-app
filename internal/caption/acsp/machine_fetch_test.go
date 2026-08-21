package acsp

import (
	"context"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/caption"
	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/caption/resolver"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// hangingFetcher models an unresponsive media block: it never answers, it just
// waits for the caller to give up.
type hangingFetcher struct{}

func (hangingFetcher) Fetch(ctx context.Context, _ string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func hangingMachine(t *testing.T, opts Options) *Machine {
	t.Helper()
	hub := fanout.NewHub(fanout.Options{})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	t.Cleanup(ctrl.Close)
	res := resolver.NewPluggable(hangingFetcher{}, nil, nil)
	opts.Controller = ctrl
	opts.Resolver = res
	opts.Loader = resolver.NewLoader(resolver.NewPipeline(res, resolver.NewCache(1<<20), nil), 0)
	return New(opts)
}

// TestHungFetchCannotOutliveTheLease: asset fetches run on the same goroutine as
// the lease and comms checks, so a media block that never answers must not stall
// them — the lease has to expire on schedule and captions have to blank (§5.2
// items 1-2). Before the fetch deadline existed this call blocked forever.
func TestHungFetchCannotOutliveTheLease(t *testing.T) {
	m := hangingMachine(t, Options{})
	m.Handle(Request{Type: ReqAnnounce, ServerID: "srv"})

	const leaseMS = 300
	granted := time.Now()
	m.Handle(Request{Type: ReqGetNewLease, LeaseMS: leaseMS})
	m.Handle(Request{Type: ReqUpdateTimeline, TimelineMS: 0, PlayoutID: "1", PlayState: PlayPlaying})

	// The RPL parses inline; the timed-text fetch behind it hangs.
	resp := m.Handle(Request{Type: ReqSetRPLLocation, RPLInline: rplDoc()})
	elapsed := time.Since(granted)
	if !resp.OK {
		t.Fatalf("Set RPL Location = %+v, want OK (the RPL itself parsed)", resp)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("hung fetch held the request path for %v; it must be bounded by the lease", elapsed)
	}

	// Wait out the remainder of the lease, then the periodic check must fire.
	if rem := time.Duration(leaseMS)*time.Millisecond - elapsed; rem > 0 {
		time.Sleep(rem + 20*time.Millisecond)
	}
	m.Tick()
	if got := m.State().State; got != model.StateLeaseExpired {
		t.Fatalf("state = %s, want LEASE_EXPIRED — the lease check was starved", got)
	}
	if m.State().LeaseActive {
		t.Fatalf("lease still active after expiry")
	}
}

// TestHungFetchBoundedByFetchTimeout: with no lease to clamp against, the
// configured fetch deadline is what bounds an unresponsive media block.
func TestHungFetchBoundedByFetchTimeout(t *testing.T) {
	m := hangingMachine(t, Options{FetchTimeout: 50 * time.Millisecond})
	start := time.Now()
	resp := m.Handle(Request{Type: ReqSetRPLLocation, RPLLocation: "rpl.xml"})
	elapsed := time.Since(start)
	if resp.OK {
		t.Fatalf("expected the unresolvable RPL fetch to fail, got %+v", resp)
	}
	if resp.Reason != "rpl_fetch_failed" {
		t.Fatalf("reason = %q, want rpl_fetch_failed", resp.Reason)
	}
	if elapsed > time.Second {
		t.Fatalf("RPL fetch took %v; FetchTimeout must bound it", elapsed)
	}
}
