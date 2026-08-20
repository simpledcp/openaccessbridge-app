package web

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
)

func newTestServer(t *testing.T, profile model.FirewallProfile, hub *fanout.Hub) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile:         profile,
		PatronBind:      "0.0.0.0:0",
		AdminBind:       "127.0.0.1:0",
		Auditorium:      "Auditorium 3",
		DefaultLanguage: "en",
	}, Deps{Hub: hub})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s
}

func TestBindSeparationEnforcedInProduction(t *testing.T) {
	// Admin on all-interfaces in a production profile must be refused.
	_, err := NewServer(Config{
		Profile:    model.ProfilePatronFacing,
		PatronBind: "0.0.0.0:8080",
		AdminBind:  "0.0.0.0:8443",
	}, Deps{Hub: fanout.NewHub(fanout.Options{})})
	if err == nil {
		t.Fatalf("expected production bind-separation error")
	}
	// Loopback admin in production is allowed.
	if _, err := NewServer(Config{
		Profile:    model.ProfilePatronFacing,
		PatronBind: "0.0.0.0:8080",
		AdminBind:  "127.0.0.1:8443",
	}, Deps{Hub: fanout.NewHub(fanout.Options{})}); err != nil {
		t.Fatalf("loopback admin should be allowed in production: %v", err)
	}
}

func TestAdminReachableFromPatron(t *testing.T) {
	cases := []struct {
		patron, admin string
		reachable     bool
	}{
		{"0.0.0.0:80", "127.0.0.1:8443", false},      // loopback admin: isolated
		{"0.0.0.0:80", "0.0.0.0:8443", true},         // admin all-interfaces: reachable
		{"192.168.1.1:80", "192.168.1.1:8443", true}, // same interface: reachable
		{"192.168.1.1:80", "10.0.0.1:8443", false},   // different interface: isolated
		{"0.0.0.0:80", "192.168.1.1:8443", true},     // patron wildcard covers admin iface
	}
	for _, c := range cases {
		if got := AdminReachableFromPatron(c.patron, c.admin); got != c.reachable {
			t.Errorf("AdminReachableFromPatron(%q,%q)=%v want %v", c.patron, c.admin, got, c.reachable)
		}
	}
}

func TestPatronMuxHasNoAdminRoutes(t *testing.T) {
	s := newTestServer(t, model.ProfileBoothDiagnostic, fanout.NewHub(fanout.Options{}))
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()
	// Admin status must not exist on the patron surface (structural separation).
	resp, err := http.Get(ts.URL + "/api/admin/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin route reachable on patron mux: status %d", resp.StatusCode)
	}
}

func TestSSEStreamsCaption(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{Auditorium: "Auditorium 3"})
	s := newTestServer(t, model.ProfileLab, hub)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/caption/stream?lang=en", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	// Give the subscriber a moment to register, then publish.
	time.Sleep(50 * time.Millisecond)
	hub.PublishCue("en", true, model.Cue{Text: "Hello SSE", ResourceID: "r1", StartMS: 1000})

	if !scanFor(resp.Body, "Hello SSE", 2*time.Second) {
		t.Fatalf("did not receive caption over SSE")
	}
}

func TestWebSocketStreamsCaptionAndSwitchesLanguage(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{})
	hub.PublishCue("en", true, model.Cue{Text: "English cue"})
	hub.PublishCue("fr", true, model.Cue{Text: "Réplique"})
	s := newTestServer(t, model.ProfileLab, hub)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/caption/ws?lang=en"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer c.CloseNow()

	// The first frame is the out-of-band language negotiation result (§7.1/D8);
	// the caption state follows.
	if got := readWSState(t, ctx, c); !strings.Contains(got, "English cue") {
		t.Fatalf("initial ws state = %q", got)
	}
	// Switch language in place without reconnect.
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"setLanguage","language":"fr"}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(readWSText(t, ctx, c), "Réplique") {
			return
		}
	}
	t.Fatalf("did not receive French cue after language switch")
}

// readWSState reads frames until a caption state frame arrives, skipping the
// web layer's own control frames (language negotiation, show end).
func readWSState(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	t.Helper()
	for i := 0; i < 8; i++ {
		got := readWSText(t, ctx, c)
		if strings.Contains(got, `"type":"state"`) {
			return got
		}
	}
	t.Fatalf("no caption state frame received")
	return ""
}

func scanFor(r interface{ Read([]byte) (int, error) }, want string, timeout time.Duration) bool {
	done := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if strings.Contains(sc.Text(), want) {
				done <- true
				return
			}
		}
		done <- false
	}()
	select {
	case ok := <-done:
		return ok
	case <-time.After(timeout):
		return false
	}
}

func readWSText(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	return string(data)
}
