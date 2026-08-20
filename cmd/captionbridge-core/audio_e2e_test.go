package main

// The §6.7 audio path proven END TO END across the three real processes.
//
// Every other test on this path exercises one half: the encoder alone, the peer
// hub alone, the relay against a loopback stand-in. This one builds and runs
// captionbridge-firewall, captionbridge-media and captionbridge-core as separate
// processes talking over their real loopback sockets, then joins as a patron
// would: it speaks exactly the protocol internal/web/assets/patron.js speaks —
// open /api/audio/ws, read the availability frame, select a stream, offer from a
// receive-only peer with NO ICE servers, apply the answer that comes back — and
// asserts that Opus RTP really arrives.
//
// It is the test that catches the failure mode this codebase has produced
// before: two halves that each pass their own tests while the integration
// silently delivers nothing.
//
// WHAT IT DOES NOT PROVE. The audio is a generated tone: this host has no
// capture hardware (BLOCKER B3). The subscriber is a Go process on the same
// machine, not a phone on a venue AP, so nothing here measures the browser
// jitter buffer that dominates §6.7's latency budget (BLOCKER B2). No assertion
// below claims either.

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/web"
)

func TestAudioPathEndToEndAcrossTheThreeProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the three appliance processes")
	}
	requireRoutableInterface(t)
	goBin := goCommand(t)
	root := repoRoot(t)

	// Short socket dir: a Unix socket path over 107 bytes fails to bind, and
	// t.TempDir()'s name is derived from this test's (long) name.
	runDir, err := os.MkdirTemp("", "cbe2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runDir) })

	bin := filepath.Join(runDir, "bin")
	for _, cmd := range []string{"captionbridge-firewall", "captionbridge-media", "captionbridge-core"} {
		build := exec.Command(goBin, "build", "-o", filepath.Join(bin, cmd), "./cmd/"+cmd)
		build.Dir = root
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, berr := build.CombinedOutput(); berr != nil {
			t.Fatalf("building %s: %v\n%s", cmd, berr, out)
		}
	}

	patronPort, adminPort := freePort(t), freePort(t)
	start(t, filepath.Join(bin, "captionbridge-firewall"), "--socket-dir="+runDir)
	start(t, filepath.Join(bin, "captionbridge-media"), "--socket-dir="+runDir)
	start(t, filepath.Join(bin, "captionbridge-core"),
		"--config", filepath.Join(runDir, "config.db"),
		"--patron-bind", fmt.Sprintf("127.0.0.1:%d", patronPort),
		"--admin-bind", fmt.Sprintf("127.0.0.1:%d", adminPort),
		"--socket-dir="+runDir)

	base := fmt.Sprintf("http://127.0.0.1:%d", patronPort)

	// 1. The patron surface must come to report playback as available. That is
	//    the whole chain answering: media is encoding, core can see its state,
	//    and the relay exists.
	doc := waitForPlayback(t, base+"/api/audio/streams", 60*time.Second)
	hi := streamNamed(t, doc, model.StreamHI)
	if !hi.Available {
		t.Fatalf("HI was never offered to a patron: %+v", doc)
	}
	// THE HONESTY LINE (B3): the source here is a generated tone and the surface
	// must keep saying so even though the audio is real.
	if !hi.TestTone {
		t.Fatal("a synthetic source reached the patron surface without being declared a test tone")
	}

	// 2. Join exactly as the patron page does.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/api/audio/ws", patronPort)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("patron audio socket: %v", err)
	}
	defer conn.CloseNow()

	if first := readFrame(t, ctx, conn); first["type"] != "audio_state" {
		t.Fatalf("first frame on the audio socket = %v, want audio_state", first["type"])
	}

	pc, packets, codec := patronPeer(t)
	sendFrame(t, ctx, conn, web.SignalFrame{Type: "select", Stream: model.StreamHI})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		j := c.ToJSON()
		idx := 0
		if j.SDPMLineIndex != nil {
			idx = int(*j.SDPMLineIndex)
		}
		mid := ""
		if j.SDPMid != nil {
			mid = *j.SDPMid
		}
		_ = writeFrame(ctx, conn, web.SignalFrame{
			Type: "ice", Stream: model.StreamHI, Candidate: j.Candidate,
			SDPMid: mid, SDPMLineIndex: idx,
		})
	})
	sendFrame(t, ctx, conn, web.SignalFrame{Type: "offer", Stream: model.StreamHI, SDP: offer.SDP})

	// 3. The appliance answers.
	var answer string
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) && answer == "" {
		msg := readFrame(t, ctx, conn)
		switch msg["type"] {
		case "answer":
			answer, _ = msg["sdp"].(string)
		case "unavailable":
			t.Fatalf("the appliance refused the offer: %v", msg["reason"])
		}
	}
	if answer == "" {
		t.Fatal("no answer came back over the patron socket")
	}
	// §6.7: host candidates only — no STUN, no TURN, nothing off the venue LAN.
	for _, forbidden := range []string{"typ srflx", "typ relay", "stun:", "turn:"} {
		if strings.Contains(answer, forbidden) {
			t.Fatalf("the appliance's answer carries %q: the local-only path reached off-LAN", forbidden)
		}
	}
	if !strings.Contains(answer, "typ host") {
		t.Fatal("the appliance's answer carries no host candidate")
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		t.Fatalf("applying the answer: %v", err)
	}

	// 4. THE PROOF: Opus RTP actually arrives at this subscriber.
	const want = 25 // a quarter second of §6.7's 10 ms frames
	got := 0
	audioDeadline := time.After(30 * time.Second)
	for got < want {
		select {
		case p := <-packets:
			if len(p.Payload) == 0 {
				t.Fatal("an RTP packet arrived with no Opus payload")
			}
			got++
		case <-audioDeadline:
			t.Fatalf("only %d RTP packets reached the subscriber: the end-to-end path is not delivering audio", got)
		}
	}
	if mime := <-codec; !strings.EqualFold(mime, webrtc.MimeTypeOpus) {
		t.Fatalf("the track a patron received is %q, not Opus", mime)
	}
	t.Logf("received %d Opus RTP packets over the real three-process path", got)
}

// patronPeer builds the browser half exactly as assets/patron.js does: a
// receive-only audio peer with an EMPTY ICE server list.
func patronPeer(t *testing.T) (*webrtc.PeerConnection, <-chan *rtpPacket, <-chan string) {
	t.Helper()
	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		t.Fatal(err)
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly}); err != nil {
		t.Fatal(err)
	}
	packets := make(chan *rtpPacket, 512)
	codec := make(chan string, 1)
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		select {
		case codec <- tr.Codec().MimeType:
		default:
		}
		for {
			p, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			select {
			case packets <- &rtpPacket{Payload: p.Payload}:
			default:
			}
		}
	})
	return pc, packets, codec
}

type rtpPacket struct{ Payload []byte }

// --- process and protocol helpers ----------------------------------------

func start(t *testing.T, bin string, args ...string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", filepath.Base(bin), err)
	}
	t.Cleanup(func() {
		// By PID: `pkill -x captionbridge-core` matches nothing, because Linux
		// truncates a process name to 15 characters.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

func waitForPlayback(t *testing.T, url string, within time.Duration) web.PatronAudio {
	t.Helper()
	deadline := time.Now().Add(within)
	var last web.PatronAudio
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			var doc web.PatronAudio
			derr := json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			if derr == nil {
				last = doc
				if doc.PlaybackAvailable {
					return doc
				}
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("the appliance never reported playback as available: %+v", last)
	return last
}

func streamNamed(t *testing.T, doc web.PatronAudio, name model.StreamType) web.PatronStream {
	t.Helper()
	for _, s := range doc.Streams {
		if s.Stream == name {
			return s
		}
	}
	t.Fatalf("%s is not in the patron document: %+v", name, doc)
	return web.PatronStream{}
}

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) map[string]any {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("reading from the patron audio socket: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("malformed frame on the patron audio socket: %v", err)
	}
	return m
}

func sendFrame(t *testing.T, ctx context.Context, c *websocket.Conn, f web.SignalFrame) {
	t.Helper()
	if err := writeFrame(ctx, c, f); err != nil {
		t.Fatalf("writing to the patron audio socket: %v", err)
	}
}

func writeFrame(ctx context.Context, c *websocket.Conn, f web.SignalFrame) error {
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return c.Write(ctx, websocket.MessageText, b)
}

func goCommand(t *testing.T) string {
	t.Helper()
	if p := filepath.Join(runtime.GOROOT(), "bin", "go"); fileExists(p) {
		return p
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain on PATH: the three appliance processes cannot be built")
	}
	return p
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// repoRoot walks up from this package to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		if fileExists(filepath.Join(dir, "go.mod")) {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the module root from the test's working directory")
	return ""
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// requireRoutableInterface skips when this host has no non-loopback address:
// pion gathers host candidates on real interfaces only, as an appliance on a
// patron LAN should, so with nothing but `lo` there is no candidate for the
// appliance and a local subscriber to meet on.
func requireRoutableInterface(t *testing.T) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		if addrs, aerr := i.Addrs(); aerr == nil && len(addrs) > 0 {
			return
		}
	}
	t.Skip("no non-loopback interface: the appliance and a local subscriber have no host candidate to meet on")
}
