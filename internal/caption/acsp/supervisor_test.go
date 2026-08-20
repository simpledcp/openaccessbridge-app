package acsp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/resolver"
	"github.com/simpledcp/captionbridge/internal/model"
)

// fakeConnector hands out in-process links in place of the Phase 0A TCP codec
// (BLOCKER B1) and publishes each server side so a test can drive or drop it.
type fakeConnector struct {
	links chan *ServerLink
	addrs chan string
}

func newFakeConnector() *fakeConnector {
	return &fakeConnector{links: make(chan *ServerLink, 8), addrs: make(chan string, 8)}
}

func (f *fakeConnector) Connect(_ context.Context, addr string) (Transport, error) {
	acs, srv := NewChanPair(8)
	f.addrs <- addr
	f.links <- srv
	return acs, nil
}

func (f *fakeConnector) next(t *testing.T, what string) *ServerLink {
	t.Helper()
	select {
	case l := <-f.links:
		return l
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return nil
	}
}

func newTestMachine(t *testing.T, files map[string][]byte) *Machine {
	t.Helper()
	hub := fanout.NewHub(fanout.Options{})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	t.Cleanup(ctrl.Close)
	fetcher := &resolver.MemoryFetcher{Files: files}
	res := resolver.NewPluggable(fetcher, nil, nil)
	loader := resolver.NewLoader(resolver.NewPipeline(res, resolver.NewCache(1<<20), nil), 0)
	return New(Options{Controller: ctrl, Resolver: res, Loader: loader})
}

func newTestSupervisor(t *testing.T, m *Machine, c Connector) *Supervisor {
	t.Helper()
	s, err := NewSupervisor(SupervisorOptions{
		Machine: m, ServerAddr: "192.0.2.10:4170", Connector: c,
		MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond,
		Sleep: func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatalf("NewSupervisor: %v", err)
	}
	return s
}

// TestSupervisorRedialsAfterTransportDrop: a dropped link must be re-opened and the
// full connection sequence re-run (§5.2 state diagram DISCONNECTED → CONNECTED).
func TestSupervisorRedialsAfterTransportDrop(t *testing.T) {
	m := newTestMachine(t, map[string][]byte{"tt_en.xml": ttDoc("en", "Live cue")})
	conn := newFakeConnector()
	sup := newTestSupervisor(t, m, conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	srv := conn.next(t, "the first link")
	if got := <-conn.addrs; got != sup.Addr() {
		t.Fatalf("dialed %q, want the allowlisted %q", got, sup.Addr())
	}
	driveSequence(t, srv)
	if got := m.State().State; got != model.StatePlaying {
		t.Fatalf("state on first link = %s, want PLAYING", got)
	}

	// The link drops mid-show; the supervisor must re-dial and re-run the sequence.
	srv.Drop()
	srv2 := conn.next(t, "the re-dial after a transport drop")
	driveSequence(t, srv2)
	if got := m.State().State; got != model.StatePlaying {
		t.Fatalf("state after re-dial = %s, want PLAYING", got)
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop on context cancel")
	}
}

// TestSupervisorSelfHealsOnLeaseExpiry: on a lease expiry the ACS must attempt the
// full new-connection sequence rather than sitting in a dead state (§5.2 item 6).
// The server here goes silent after the lease is granted, so only the self-heal
// path can produce a second connection.
func TestSupervisorSelfHealsOnLeaseExpiry(t *testing.T) {
	m := newTestMachine(t, map[string][]byte{"tt_en.xml": ttDoc("en", "Live cue")})
	conn := newFakeConnector()
	sup := newTestSupervisor(t, m, conn)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.Run(ctx) }()

	srv := conn.next(t, "the first link")
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	for _, req := range []Request{
		{Type: ReqAnnounce, ServerID: "srv", ScreenName: "Room"},
		{Type: ReqGetNewLease, LeaseMS: 250}, // a very short lease, then silence
	} {
		if err := srv.Send(sctx, req); err != nil {
			t.Fatalf("send %s: %v", req.Type, err)
		}
		if _, err := srv.Recv(sctx); err != nil {
			t.Fatalf("recv ack for %s: %v", req.Type, err)
		}
	}
	// No keepalive: the lease lapses, the machine blanks and asks for a re-run.
	srv2 := conn.next(t, "the self-heal re-connection after lease expiry")
	driveSequence(t, srv2)
	if got := m.State().State; got != model.StatePlaying {
		t.Fatalf("state after self-heal = %s, want PLAYING", got)
	}
	if !m.State().LeaseActive {
		t.Fatalf("expected a live lease after the self-heal sequence")
	}
}

// driveSequence runs the canonical connection sequence over a server link.
func driveSequence(t *testing.T, srv *ServerLink) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, req := range []Request{
		{Type: ReqAnnounce, ServerID: "srv", ScreenName: "Room"},
		{Type: ReqGetNewLease, LeaseMS: 30000},
		{Type: ReqUpdateTimeline, TimelineMS: 1000, PlayoutID: "1", PlayState: PlayPlaying},
		{Type: ReqSetRPLLocation, RPLInline: rplDoc()},
		{Type: ReqSetOutputMode, OutputEnabled: BoolPtr(true)},
	} {
		if err := srv.Send(ctx, req); err != nil {
			t.Fatalf("send %s: %v", req.Type, err)
		}
		if _, err := srv.Recv(ctx); err != nil {
			t.Fatalf("recv ack for %s: %v", req.Type, err)
		}
	}
}

// TestSupervisorRejectsUnusableAddress: a misconfigured server address must fail
// loudly at construction, never as a supervisor that quietly never connects.
func TestSupervisorRejectsUnusableAddress(t *testing.T) {
	m := newTestMachine(t, nil)
	cases := []struct {
		name string
		addr string
	}{
		{"empty", ""},
		{"no port", "192.0.2.10"},
		{"port zero", "192.0.2.10:0"},
		{"port out of range", "192.0.2.10:99999"},
		{"no host", ":4170"},
		{"garbage", "not an address"},
	}
	for _, tc := range cases {
		_, err := NewSupervisor(SupervisorOptions{
			Machine: m, ServerAddr: tc.addr, Connector: newFakeConnector(),
		})
		if err == nil {
			t.Errorf("%s: expected an error for address %q", tc.name, tc.addr)
		}
	}
	if _, err := NewSupervisor(SupervisorOptions{Machine: m, ServerAddr: "192.0.2.10:4170"}); err == nil {
		t.Errorf("expected an error with no connector (the Phase 0A codec seam)")
	}
}

// TestSupervisorFromProfile: the allowlisted address and the variant come from the
// persisted auditorium profile (§5.1 / D6), not from a hardcoded default.
func TestSupervisorFromProfile(t *testing.T) {
	m := newTestMachine(t, nil)
	opts := SupervisorOptions{Machine: m, Connector: newFakeConnector()}

	sup, err := SupervisorFromProfile(model.AuditoriumProfile{
		CinemaServerIP: "10.20.30.40", ACSPPort: 4170, ACSPInitiation: model.ACSInitiated,
	}, opts)
	if err != nil {
		t.Fatalf("SupervisorFromProfile: %v", err)
	}
	if got := sup.Addr(); got != "10.20.30.40:4170" {
		t.Errorf("addr = %q, want 10.20.30.40:4170", got)
	}
	if got := sup.Initiation(); got != model.ACSInitiated {
		t.Errorf("initiation = %q, want %q", got, model.ACSInitiated)
	}
	// Variant B is a config change, not a rewrite (D6).
	subB, err := SupervisorFromProfile(model.AuditoriumProfile{
		CinemaServerIP: "10.20.30.40", ACSPPort: 4170, ACSPInitiation: model.ServerInitiated,
	}, opts)
	if err != nil {
		t.Fatalf("SupervisorFromProfile (variant B): %v", err)
	}
	if got := subB.Initiation(); got != model.ServerInitiated {
		t.Errorf("variant B initiation = %q", got)
	}
	// An unconfigured profile must not produce a supervisor.
	if _, err := SupervisorFromProfile(model.AuditoriumProfile{ACSPPort: 4170}, opts); err == nil {
		t.Errorf("expected an error with no cinema server IP")
	}
	if _, err := SupervisorFromProfile(model.AuditoriumProfile{CinemaServerIP: "10.20.30.40"}, opts); err == nil {
		t.Errorf("expected an error with no ACSP port")
	}
}
