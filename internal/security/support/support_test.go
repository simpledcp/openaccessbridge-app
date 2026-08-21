package support

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// fakeStore is the audit trail, in memory.
type fakeStore struct {
	mu        sync.Mutex
	opened    []model.SupportSession
	closed    []closeRec
	openErr   error
	stillOpen []model.SupportSession
}

type closeRec struct {
	id    string
	state model.SupportSessionState
}

func (f *fakeStore) RecordSupportOpen(s model.SupportSession) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return f.openErr
	}
	f.opened = append(f.opened, s)
	return nil
}

func (f *fakeStore) RecordSupportClose(id string, state model.SupportSessionState, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, closeRec{id, state})
	return nil
}

func (f *fakeStore) OpenSupportSessions() ([]model.SupportSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stillOpen, nil
}

func (f *fakeStore) closes() []closeRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]closeRec(nil), f.closed...)
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newManager(t *testing.T, store *fakeStore, ep Endpoint) (*Manager, *clock, *strings.Builder) {
	t.Helper()
	c := &clock{t: time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)}
	var logs syncWriter
	m := New(Options{
		Store:    store,
		Log:      logging.New(logging.Options{Component: "test", JSON: true, Writer: &logs}),
		Endpoint: func() Endpoint { return ep },
		Now:      c.now,
	})
	return m, c, &logs.b
}

type syncWriter struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func goodEndpoint() Endpoint { return Endpoint{Iface: "eth0", Host: "203.0.113.10", Port: 443} }

// §9.4: off by default. Nothing opens a window without an operator doing it.
func TestSupportModeIsOffUntilSomeoneOpensIt(t *testing.T) {
	m, _, _ := newManager(t, &fakeStore{}, goodEndpoint())
	if _, ok := m.Current(); ok {
		t.Fatal("a session exists on a freshly built manager")
	}
	if m.Egress() != nil {
		t.Fatal("the firewall would carry a support window with no session open")
	}
	if rep := m.Status(); rep.Active {
		t.Fatal("status reports an active session on a fresh manager")
	}
}

// An appliance with no configured endpoint cannot be talked into opening a
// window to nowhere — the only ways to render that are an unrestricted egress
// hole or silence.
func TestASessionCannotBeOpenedWithoutAnEndpoint(t *testing.T) {
	for name, ep := range map[string]Endpoint{
		"nothing":      {},
		"no host":      {Iface: "eth0", Port: 443},
		"no interface": {Host: "203.0.113.10", Port: 443},
		"no port":      {Iface: "eth0", Host: "203.0.113.10"},
	} {
		m, _, _ := newManager(t, &fakeStore{}, ep)
		if _, err := m.Open(OpenRequest{Reason: "ticket 1", AdminUser: "tech"}); !errors.Is(err, ErrNoEndpoint) {
			t.Errorf("%s: opened with err %v, want ErrNoEndpoint", name, err)
		}
	}
}

// §9.4 audits sessions. An audit entry with no reason is a timestamp.
func TestASessionRequiresAReason(t *testing.T) {
	m, _, _ := newManager(t, &fakeStore{}, goodEndpoint())
	if _, err := m.Open(OpenRequest{AdminUser: "tech"}); !errors.Is(err, ErrNoReason) {
		t.Fatalf("opened with no reason: %v", err)
	}
	if _, err := m.Open(OpenRequest{Reason: "   ", AdminUser: "tech"}); !errors.Is(err, ErrNoReason) {
		t.Fatalf("whitespace passed as a reason: %v", err)
	}
}

// "Time-limited" means a limit an operator cannot raise.
func TestTheWindowIsClampedToTheMaximum(t *testing.T) {
	m, c, _ := newManager(t, &fakeStore{}, goodEndpoint())
	sess, err := m.Open(OpenRequest{Reason: "long call", AdminUser: "tech", Duration: 30 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := sess.ExpiresAt.Sub(c.now()); got != model.MaxSupportSession {
		t.Fatalf("a 30-day request produced a %s window, want %s", got, model.MaxSupportSession)
	}
}

// The window closes itself. A session that has to be closed by hand is one that
// can be forgotten, and a forgotten window on an appliance with SSH disabled is
// a permanent way in.
func TestTheWindowExpiresOnItsOwn(t *testing.T) {
	store := &fakeStore{}
	m, c, _ := newManager(t, store, goodEndpoint())
	if _, err := m.Open(OpenRequest{Reason: "ticket 7", AdminUser: "tech", Duration: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if m.Egress() == nil {
		t.Fatal("no egress window while a session is open")
	}

	c.advance(time.Hour + time.Second)

	if _, ok := m.Current(); ok {
		t.Fatal("an expired session is still reported as current")
	}
	if m.Egress() != nil {
		t.Fatal("the firewall would still carry the window of an expired session")
	}
	var found bool
	for _, rec := range store.closes() {
		if rec.state == model.SupportExpired {
			found = true
		}
	}
	if !found {
		t.Fatalf("the expiry was not recorded in the audit trail: %+v", store.closes())
	}
}

// Support mode fails closed across a restart: a window that survived a boot would
// outlive the person who opened it and the reason they gave.
func TestAnOpenSessionDoesNotSurviveARestart(t *testing.T) {
	store := &fakeStore{stillOpen: []model.SupportSession{{
		ID: "sup-old", State: model.SupportOpen,
		OpenedAt: time.Now().Add(-time.Minute), ExpiresAt: time.Now().Add(time.Hour),
	}}}
	m, _, _ := newManager(t, store, goodEndpoint())

	if _, ok := m.Current(); ok {
		t.Fatal("a session from before the restart came back open")
	}
	if m.Egress() != nil {
		t.Fatal("the firewall would carry a window inherited from before a restart")
	}
	closes := store.closes()
	if len(closes) != 1 || closes[0].state != model.SupportEndedByRestart {
		t.Fatalf("the restart was not recorded as the ending: %+v", closes)
	}
}

// One at a time. Extending is deliberately close + open, so the trail shows two
// windows with two reasons rather than one that silently grew.
func TestASecondSessionIsRefusedWhileOneIsOpen(t *testing.T) {
	m, _, _ := newManager(t, &fakeStore{}, goodEndpoint())
	if _, err := m.Open(OpenRequest{Reason: "first", AdminUser: "tech"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(OpenRequest{Reason: "second", AdminUser: "tech"}); !errors.Is(err, ErrAlreadyOpen) {
		t.Fatalf("a second session opened: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(OpenRequest{Reason: "third", AdminUser: "tech"}); err != nil {
		t.Fatalf("could not reopen after closing: %v", err)
	}
}

// An audited feature whose audit failed is an unaudited feature. If the trail
// cannot record the session, the window does not open.
func TestAWindowIsNotOpenedIfTheTrailCannotRecordIt(t *testing.T) {
	store := &fakeStore{openErr: errors.New("disk full")}
	m, _, _ := newManager(t, store, goodEndpoint())
	if _, err := m.Open(OpenRequest{Reason: "ticket 9", AdminUser: "tech"}); err == nil {
		t.Fatal("a session opened without being recorded")
	}
	if m.Egress() != nil {
		t.Fatal("the firewall carries a window for a session that was never recorded")
	}
}

// §9.4: the operator's reason is free text and must not reach the log. Its
// presence and length may, so an empty-reason trail is visible without the text.
func TestTheReasonIsAuditedLocallyAndNeverLogged(t *testing.T) {
	store := &fakeStore{}
	m, _, logs := newManager(t, store, goodEndpoint())
	const secret = "caller said the password is hunter2"
	if _, err := m.Open(OpenRequest{Reason: secret, AdminUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	if strings.Contains(out, secret) || strings.Contains(out, "hunter2") {
		t.Fatalf("the operator's reason reached the log:\n%s", out)
	}
	if strings.Contains(out, "alice") {
		t.Fatalf("the admin username reached the log unhashed:\n%s", out)
	}
	if !strings.Contains(out, "reason_len") {
		t.Fatalf("the log does not record that a reason was given at all:\n%s", out)
	}
	// The trail keeps it: that is the venue's own record.
	if len(store.opened) != 1 || store.opened[0].Reason != secret {
		t.Fatal("the audit trail did not keep the reason")
	}
}

// A window is not a connection. This appliance permits the egress; it dials
// nothing, and every surface must be able to say so.
func TestAnOpenWindowIsNeverReportedAsAnEstablishedConnection(t *testing.T) {
	m, _, _ := newManager(t, &fakeStore{}, goodEndpoint())
	if _, err := m.Open(OpenRequest{Reason: "ticket 3", AdminUser: "tech"}); err != nil {
		t.Fatal(err)
	}
	rep := m.Status()
	if !rep.Active {
		t.Fatal("the window is open and status says otherwise")
	}
	if rep.TunnelEstablished {
		t.Fatal("the appliance claims a support tunnel is established; it has no tunnel client")
	}
	if !strings.Contains(rep.Note, "it does not make one") {
		t.Fatalf("the note does not distinguish permitting a connection from making one: %q", rep.Note)
	}
}

// An appliance with no endpoint and SSH disabled has no way back in. That is
// worth knowing before it is needed rather than during an outage.
func TestAnApplianceWithNoEndpointSaysItCannotBeReEntered(t *testing.T) {
	m, _, _ := newManager(t, &fakeStore{}, Endpoint{})
	rep := m.Status()
	if rep.EndpointConfigured {
		t.Fatal("an unconfigured endpoint reports as configured")
	}
	if !strings.Contains(rep.Note, "no way back into this appliance") {
		t.Fatalf("the note does not warn that there is no re-entry path: %q", rep.Note)
	}
}

// Reconfiguring the endpoint out from under an open session must permit nothing,
// rather than keep permitting the old address.
func TestAWindowFollowsTheConfiguredEndpointAndNotAStaleOne(t *testing.T) {
	ep := goodEndpoint()
	c := &clock{t: time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)}
	m := New(Options{
		Store: &fakeStore{}, Endpoint: func() Endpoint { return ep }, Now: c.now,
	})
	if _, err := m.Open(OpenRequest{Reason: "ticket 5", AdminUser: "tech"}); err != nil {
		t.Fatal(err)
	}
	if m.Egress() == nil {
		t.Fatal("no window while open")
	}
	ep = Endpoint{} // the operator cleared it
	if g := m.Egress(); g != nil {
		t.Fatalf("the window still permits %+v after the endpoint was cleared", g)
	}
}

// Opening and closing must prompt the supervisor immediately: an expired window
// that waits for the next scheduled posture pass is open past its deadline, and
// the deadline is the promise.
func TestOpeningAndClosingSignalTheSupervisor(t *testing.T) {
	var mu sync.Mutex
	changes := 0
	c := &clock{t: time.Date(2026, 8, 21, 20, 0, 0, 0, time.UTC)}
	m := New(Options{
		Store: &fakeStore{}, Endpoint: goodEndpoint, Now: c.now,
		OnChange: func() { mu.Lock(); changes++; mu.Unlock() },
	})
	if _, err := m.Open(OpenRequest{Reason: "ticket 2", AdminUser: "tech"}); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := changes
	mu.Unlock()
	if got < 2 {
		t.Fatalf("open+close signalled the supervisor %d times, want at least 2", got)
	}
}
