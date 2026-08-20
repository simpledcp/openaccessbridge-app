package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
)

// loopbackSignaler stands in for captionbridge-media's peer process. It exercises
// the relay, the peer-identity plumbing and the picker state machine; it claims
// NOTHING about real media, and no test here asserts that audio was heard. The
// real path is proven across the three processes in cmd/captionbridge-core.
type loopbackSignaler struct {
	mu      sync.Mutex
	seen    []SignalFrame
	peerIDs []string
}

func (l *loopbackSignaler) frames() []SignalFrame {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]SignalFrame(nil), l.seen...)
}

func (l *loopbackSignaler) peers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.peerIDs...)
}

func (l *loopbackSignaler) Signal(_ context.Context, peerID string, in SignalFrame) ([]SignalFrame, error) {
	l.mu.Lock()
	l.seen = append(l.seen, in)
	l.peerIDs = append(l.peerIDs, peerID)
	l.mu.Unlock()
	switch in.Type {
	case "offer":
		return []SignalFrame{{Type: "answer", Stream: in.Stream, SDP: "v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\n"}}, nil
	case "ice":
		return []SignalFrame{{Type: "ice", Stream: in.Stream, Candidate: "candidate:1 1 UDP 1 127.0.0.1 5000 typ host"}}, nil
	}
	return nil, nil
}

// hiState is a healthy HI stream: a connected input that qualifies AND a live
// encoder, because both are required before a patron is offered the feed.
func hiState(f func(*model.AudioStreamState)) model.AudioStreamState {
	st := model.AudioStreamState{
		StreamType: model.StreamHI, Present: true, DeviceConnected: true,
		Status: model.QualExcellent, EncoderState: model.EncoderStateStreaming,
	}
	if f != nil {
		f(&st)
	}
	return st
}

// The patron-safe projection: availability plus one calm sentence, and never a
// device name, a level, or a qualifier reason (§7.2 no diagnostic leakage).
func TestPatronStreamProjection(t *testing.T) {
	cases := []struct {
		name      string
		states    []model.AudioStreamState
		wantAvail bool
		wantCopy  string
	}{
		{"not configured at all", nil, false, "Hearing assistance audio is not currently available."},
		{"device unplugged", []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
			s.DeviceConnected = false
		})}, false, "not currently available"},
		{"qualifier failed", []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
			s.Status = model.QualFailed
			s.Reasons = []string{"input appears mono, but this profile requires two channels"}
		})}, false, "not currently available"},
		{"sustained silence is the too-low state", []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
			s.Silence = true
		})}, false, "signal is too low"},
		{"marginal is offered WITH the caution", []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
			s.Status = model.QualMarginal
		})}, true, "may be uneven"},
		// §6.6's encoder state decides whether a perfect input can actually
		// reach a phone. Without a live encode there is nothing to offer, however
		// clean the booth's feed measures.
		{"no encoder means nothing can be delivered", []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
			s.EncoderState = model.EncoderStateUnavailable
		})}, false, "not currently available"},
		{"an encoder that only smoke-tested is not a transport", []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
			s.EncoderState = model.EncoderStateReadyNoTransport
		})}, false, "not currently available"},
		{"healthy", []model.AudioStreamState{hiState(nil)}, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := projectStreams(c.states, nil)
			if len(got) != 2 {
				t.Fatalf("expected both named streams, got %d", len(got))
			}
			hi := got[0]
			if hi.Stream != model.StreamHI {
				t.Fatalf("first stream = %q", hi.Stream)
			}
			if hi.Available != c.wantAvail {
				t.Fatalf("available = %v want %v (reason %q)", hi.Available, c.wantAvail, hi.Reason)
			}
			if c.wantCopy != "" && !strings.Contains(hi.Reason, c.wantCopy) {
				t.Fatalf("reason %q does not carry %q", hi.Reason, c.wantCopy)
			}
			for _, leak := range []string{"mono", "requires", "dBFS", "usb", "encoder"} {
				if strings.Contains(strings.ToLower(hi.Reason), leak) {
					t.Fatalf("patron copy %q leaks %q", hi.Reason, leak)
				}
			}
		})
	}
}

// A session that was granted HI only must not be offered VI-N (§10.4).
func TestGrantedModesLimitTheOfferedStreams(t *testing.T) {
	states := []model.AudioStreamState{hiState(nil), {
		StreamType: model.StreamVIN, Present: true, DeviceConnected: true,
		Status: model.QualExcellent, EncoderState: model.EncoderStateStreaming,
	}}
	sess := model.PatronSession{GrantedModes: []model.PatronMode{model.ModeCaption, model.ModeHI}}
	got := projectStreams(states, func(s model.StreamType) bool {
		m, ok := model.ModeForStream(s)
		return ok && sess.Grants(m)
	})
	if len(got) != 1 || got[0].Stream != model.StreamHI {
		t.Fatalf("granted-mode filter produced %+v", got)
	}
}

func newAudioServer(t *testing.T, sig AudioSignaler, states []model.AudioStreamState) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile: model.ProfileLab, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
		Auditorium: "Auditorium 3",
	}, Deps{
		Hub:      fanout.NewHub(fanout.Options{}),
		Audio:    func() []model.AudioStreamState { return states },
		Signaler: sig,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// With no relay to the media process the page must be told plainly that
// listening is not
// available — never shown a player that cannot play (guardrail #7, B4/B2).
func TestNoSignalingPeerIsReportedHonestly(t *testing.T) {
	s := newAudioServer(t, nil, []model.AudioStreamState{hiState(nil)})
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	var doc PatronAudio
	if err := json.Unmarshal([]byte(getBody(t, ts.URL+"/api/audio/streams")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.PlaybackAvailable {
		t.Fatalf("playback reported available with no signaling peer")
	}
	if !strings.Contains(doc.PlaybackReason, "not available") {
		t.Fatalf("playback reason %q is not a plain patron message", doc.PlaybackReason)
	}

	frames := dialAudio(t, ts, []SignalFrame{{Type: "select", Stream: model.StreamHI}})
	if len(frames) == 0 || frames[len(frames)-1].Type != "unavailable" {
		t.Fatalf("selecting a stream with no peer = %+v want an unavailable frame", frames)
	}
}

// §6.7: "SDP offer/answer + ICE are exchanged over the existing local web server's
// WebSocket — no third-party signaling."
func TestAudioSignalingRelaysOfferAndICE(t *testing.T) {
	sig := &loopbackSignaler{}
	s := newAudioServer(t, sig, []model.AudioStreamState{hiState(nil)})
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	frames := dialAudio(t, ts, []SignalFrame{
		{Type: "offer", Stream: model.StreamHI, SDP: "v=0\r\n"},
		{Type: "ice", Stream: model.StreamHI, Candidate: "candidate:2 1 UDP 1 10.0.0.9 5001 typ host"},
	})
	var sawAnswer, sawICE bool
	for _, f := range frames {
		switch f.Type {
		case "answer":
			sawAnswer = true
		case "ice":
			sawICE = true
		}
	}
	if !sawAnswer || !sawICE {
		t.Fatalf("relay produced %+v; want an answer and an ICE candidate", frames)
	}
}

// A stream the appliance cannot serve must be refused with patron copy even when a
// peer exists — availability is checked before the media process is ever reached.
func TestUnavailableStreamIsRefusedBeforeReachingMedia(t *testing.T) {
	sig := &loopbackSignaler{}
	s := newAudioServer(t, sig, []model.AudioStreamState{hiState(nil)}) // no VI-N configured
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	frames := dialAudio(t, ts, []SignalFrame{{Type: "offer", Stream: model.StreamVIN, SDP: "v=0\r\n"}})
	if len(frames) == 0 || frames[len(frames)-1].Type != "unavailable" {
		t.Fatalf("VI-N offer = %+v want unavailable", frames)
	}
	if !strings.Contains(frames[len(frames)-1].Reason, "Audio description") {
		t.Fatalf("refusal copy %q does not name the feed in patron language", frames[len(frames)-1].Reason)
	}
	for _, seen := range sig.frames() {
		if seen.Type == "offer" {
			t.Fatalf("an offer for an unavailable stream reached the media process")
		}
	}
}

// The flip side of the "no peer" state: with a relay AND a live encoder the
// patron surface must say listening IS available, or a working appliance keeps
// telling patrons to contact staff.
func TestPlaybackIsAvailableWhenAStreamIsReallyBeingEncoded(t *testing.T) {
	s := newAudioServer(t, &loopbackSignaler{}, []model.AudioStreamState{hiState(nil)})
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	var doc PatronAudio
	if err := json.Unmarshal([]byte(getBody(t, ts.URL+"/api/audio/streams")), &doc); err != nil {
		t.Fatal(err)
	}
	if !doc.PlaybackAvailable {
		t.Fatalf("playback reported unavailable with a live encoder and a relay: %+v", doc)
	}
	if doc.PlaybackReason != "" {
		t.Fatalf("an available surface still carries the unavailable copy %q", doc.PlaybackReason)
	}
}

// A relay with nothing being encoded is not playback. The whole-surface state
// has to follow the streams, not merely the presence of a signaling peer.
func TestPlaybackIsUnavailableWhenNothingIsEncoding(t *testing.T) {
	states := []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
		s.EncoderState = model.EncoderStateUnavailable
	})}
	s := newAudioServer(t, &loopbackSignaler{}, states)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	var doc PatronAudio
	if err := json.Unmarshal([]byte(getBody(t, ts.URL+"/api/audio/streams")), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.PlaybackAvailable {
		t.Fatal("playback reported available while nothing was being encoded")
	}
	if !strings.Contains(doc.PlaybackReason, "not available") {
		t.Fatalf("playback reason %q is not the plain patron message", doc.PlaybackReason)
	}
}

// THE HONESTY LINE (BLOCKER B3). A patron can now really receive this stream, so
// the surface must say that what they will hear is a generated tone rather than
// this auditorium's feed — while still carrying no device name or diagnostic.
func TestASyntheticSourceIsDeclaredToThePatron(t *testing.T) {
	states := []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
		s.InputDeviceID = model.SyntheticDevicePrefix + "HI"
	})}
	got := projectStreams(states, nil)
	if !got[0].TestTone {
		t.Fatal("a synthetic stand-in reached the patron surface without being declared a test tone")
	}
	if !got[0].Available {
		t.Fatal("a test tone that is really being encoded should still be offered — honestly labelled, not hidden")
	}
	// The declaration must not smuggle the diagnostic in with it.
	for _, leak := range []string{"synthetic-", "device", "dbfs"} {
		if strings.Contains(strings.ToLower(got[0].Reason), leak) {
			t.Fatalf("patron copy %q leaks %q", got[0].Reason, leak)
		}
	}

	real := []model.AudioStreamState{hiState(func(s *model.AudioStreamState) {
		s.InputDeviceID = "usb-Generic_USB_Audio-00"
	})}
	if projectStreams(real, nil)[0].TestTone {
		t.Fatal("a real capture device was labelled a test tone")
	}
}

// §6.7's peers "join and leave mid-show". Every frame from one patron socket
// must carry the SAME opaque peer id, and closing the socket must release that
// peer — otherwise a phone that walked out holds a peer connection for the show.
func TestOnePeerPerSocketAndItIsReleasedOnClose(t *testing.T) {
	sig := &loopbackSignaler{}
	s := newAudioServer(t, sig, []model.AudioStreamState{hiState(nil)})
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	dialAudio(t, ts, []SignalFrame{
		{Type: "offer", Stream: model.StreamHI, SDP: "v=0\r\n"},
		{Type: "ice", Stream: model.StreamHI, Candidate: "candidate:2 1 UDP 1 10.0.0.9 5001 typ host"},
	})

	// The close is relayed from the socket's own teardown, after dialAudio's
	// connection has gone.
	deadline := time.Now().Add(3 * time.Second)
	var frames []SignalFrame
	for time.Now().Before(deadline) {
		frames = sig.frames()
		if len(frames) >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(frames) < 3 || frames[len(frames)-1].Type != "close" {
		t.Fatalf("the departing patron's peer was never released: %+v", frames)
	}
	ids := sig.peers()
	if ids[0] == "" {
		t.Fatal("no peer identity was minted for the control socket")
	}
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("frame %d carried peer id %q, not the socket's own %q", i, id, ids[0])
		}
	}

	// A second patron must be a different peer, or two phones would share (and
	// tear down) one peer connection.
	sig2 := &loopbackSignaler{}
	s2 := newAudioServer(t, sig2, []model.AudioStreamState{hiState(nil)})
	ts2 := httptest.NewServer(s2.PatronHandler())
	defer ts2.Close()
	dialAudio(t, ts2, []SignalFrame{{Type: "offer", Stream: model.StreamHI, SDP: "v=0\r\n"}})
	if sig2.peers()[0] == ids[0] {
		t.Fatal("two patron sockets were given the same peer identity")
	}
}

// dialAudio opens the audio control socket, sends frames, and returns everything
// received (the leading audio_state frame is dropped).
func dialAudio(t *testing.T, ts *httptest.Server, send []SignalFrame) []SignalFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/api/audio/ws"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("audio ws dial: %v", err)
	}
	defer c.CloseNow()

	// First frame is the availability document.
	if _, _, err := c.Read(ctx); err != nil {
		t.Fatalf("audio ws first read: %v", err)
	}
	for _, f := range send {
		b, _ := json.Marshal(f)
		if err := c.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatalf("audio ws write: %v", err)
		}
	}
	var out []SignalFrame
	rctx, rcancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer rcancel()
	for len(out) < len(send) {
		_, data, err := c.Read(rctx)
		if err != nil {
			break
		}
		var f SignalFrame
		if json.Unmarshal(data, &f) != nil || f.Type == "audio_state" {
			continue
		}
		out = append(out, f)
	}
	return out
}
