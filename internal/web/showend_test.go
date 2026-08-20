package web

import (
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

// §7.2 show-end behavior: "streams terminate and the session expires at show end;
// the page shows a polite 'this show has ended' state rather than dangling."
// Previously show end reverted to "Captions are waiting for the show to start."
// and the connection stayed open indefinitely.
func TestShowEndTerminatesTheCaptionWebSocket(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{Auditorium: "Auditorium 3"})
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
	// Drain the language frame and the first state so the connection is live.
	_ = readWSState(t, ctx, c)

	s.EndShow()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := readWSText(t, ctx, c)
		if strings.Contains(got, `"`+msgShowEnded+`"`) {
			return
		}
	}
	t.Fatalf("the caption socket never delivered a show-ended state")
}

func TestShowEndTerminatesTheCaptionSSEStream(t *testing.T) {
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

	go func() {
		time.Sleep(100 * time.Millisecond)
		s.EndShow()
	}()
	if !scanFor(resp.Body, msgShowEnded, 2*time.Second) {
		t.Fatalf("the SSE stream never delivered a show-ended state")
	}
}

// The page must carry the polite copy, and it must be distinct from the
// waiting-for-the-show copy that used to stand in for it.
func TestShowEndedCopyExistsAndIsNotTheWaitingCopy(t *testing.T) {
	js, err := assetsFS.ReadFile("assets/patron.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(js)
	if !strings.Contains(src, "show_ended") {
		t.Fatalf("patron client has no show-ended state")
	}
	if !strings.Contains(src, "This show has ended") {
		t.Fatalf("patron client has no polite show-ended copy")
	}
}
