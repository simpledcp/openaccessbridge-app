package main

// The §6.7 audio path proven END TO END across the three real processes.
//
// Every other test on this path exercises one half: the encoder alone, the peer
// hub alone, the relay against a loopback stand-in. This one builds and runs
// openaccessbridge-firewall, openaccessbridge-media and openaccessbridge-core as separate
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
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
	"github.com/simpledcp/openaccessbridge/internal/audio/encode"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
	"github.com/simpledcp/openaccessbridge/internal/web"
)

func TestAudioPathEndToEndAcrossTheThreeProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the three appliance processes")
	}
	// The Opus encoder is a gst-launch subprocess (D20). Without it the media
	// process runs the Null backend, playback never becomes available, and this
	// test would spend its full timeout failing on a host that was never going
	// to pass — so skip honestly, as the encode smoke test does
	// (DEVELOPMENT.md: GStreamer is an optional runtime seam; CI installs it so
	// this path stays exercised there).
	if !encode.NewGStreamerBackend().Available() {
		t.Skip("no gst-launch-1.0/opusenc on this host: nothing can encode, so there is no §6.7 end-to-end audio path to prove")
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
	for _, cmd := range []string{"openaccessbridge-firewall", "openaccessbridge-media", "openaccessbridge-core"} {
		build := exec.Command(goBin, "build", "-o", filepath.Join(bin, cmd), "./cmd/"+cmd)
		build.Dir = root
		build.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, berr := build.CombinedOutput(); berr != nil {
			t.Fatalf("building %s: %v\n%s", cmd, berr, out)
		}
	}

	patronPort, adminPort := freePort(t), freePort(t)
	start(t, filepath.Join(bin, "openaccessbridge-firewall"), "--socket-dir="+runDir)
	mediaLog := &logTail{}
	media := startCapturing(t, mediaLog, filepath.Join(bin, "openaccessbridge-media"), "--socket-dir="+runDir)
	start(t, filepath.Join(bin, "openaccessbridge-core"),
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

	// 2. Join exactly as the patron page does, and prove Opus RTP arrives.
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	first := joinAsPatron(t, ctx, patronPort, model.StreamHI)
	got := first.waitForOpus(t, 25) // a quarter second of §6.7's 10 ms frames
	t.Logf("received %d Opus RTP packets over the real three-process path", got)

	// 3. §6.7's TOPOLOGY, checked against the real processes rather than a fake
	//    encoder: "one capture/analysis/Opus source per named stream … the design
	//    must avoid re-running capture/analysis per client." One gst-launch child
	//    per named stream is what that means on this appliance, so a SECOND
	//    subscriber to the SAME stream must add an SRTP session and nothing else.
	//    If the shape were one encoder per subscriber this count would double,
	//    and a full auditorium would fork one Opus encoder per phone.
	before := encoderChildren(t, media.Process.Pid)
	streaming := 0
	for _, ps := range waitForPlayback(t, base+"/api/audio/streams", 10*time.Second).Streams {
		if ps.Available {
			streaming++
		}
	}
	if streaming == 0 {
		t.Fatal("no stream is being offered while the appliance is delivering audio")
	}
	if len(before) != streaming {
		t.Fatalf("%d encoder children for %d streams being served: §6.7 wants exactly one Opus source per NAMED STREAM",
			len(before), streaming)
	}
	second := joinAsPatron(t, ctx, patronPort, model.StreamHI)
	second.waitForOpus(t, 25)

	// Both subscribers must be receiving: a second peer that stole the first
	// one's track would pass a naive count.
	firstBefore := first.received()
	waitUntil(t, 10*time.Second, func() bool { return first.received() > firstBefore+10 })

	after := encoderChildren(t, media.Process.Pid)
	if len(after) != len(before) {
		t.Fatalf("a second subscriber to the same stream took the encoder count from %d to %d: §6.7's source is per NAMED STREAM, not per client",
			len(before), len(after))
	}

	// 4. LIFECYCLE. A patron who closes the page — locks the phone, walks out,
	//    reloads — must leave nothing behind. Neither the encoder children, the
	//    media process's open descriptors, nor the audio the remaining
	//    subscriber is receiving may grow with the number of patrons who came
	//    and went. Each leaked peer would hold an ICE agent, a UDP socket and an
	//    SRTP session for the rest of the show.
	for i := 0; i < 6; i++ {
		reload := joinAsPatron(t, ctx, patronPort, model.StreamHI)
		reload.waitForOpus(t, 10)
		reload.close()
	}
	stillHere := first.received()
	waitUntil(t, 10*time.Second, func() bool { return first.received() > stillHere+10 })
	afterReloads := encoderChildren(t, media.Process.Pid)
	if len(afterReloads) != len(before) {
		t.Fatalf("six patron reloads took the encoder count from %d to %d: subscribers are leaking encoder subprocesses",
			len(before), len(afterReloads))
	}
	// §6.6's connected listener count, read from the media process's own
	// webrtc.peer.connected events. Two things are checked, and the LOWER bound
	// is the load-bearing one: the count must actually track the peers that
	// connected (a constant, or a number invented by the relay rather than
	// observed by the hub, cannot reach 2 or 3 here) and must never exceed the
	// number of subscribers ever connected at once — two long-lived ones plus
	// the reload in hand.
	//
	// The upper bound is a bound, not a proof of teardown: on loopback a peer
	// whose subscriber vanished is reaped in milliseconds because the closed
	// socket refuses packets, where a phone that walks out of a venue takes
	// ICE's timeouts to notice. The release path itself is pinned by
	// web.TestOnePeerPerSocketAndItIsReleasedOnClose and by
	// webrtcpeer.TestReofferReleasesThePeerItReplaces.
	const concurrent = 3
	if peak := mediaLog.peakListeners(t); peak > concurrent {
		t.Fatalf("the media process reported %d connected listeners while at most %d were ever connected: departing subscribers are leaking peers",
			peak, concurrent)
	} else if peak < 2 {
		t.Fatalf("the media process never reported more than %d connected listeners: the §6.6 count is not coming from real peers", peak)
	}

	// 4b. §9.1 / D28: what did the appliance actually TELL those phones to try?
	//     The scope is configured in core's store, applied in the media process,
	//     and measured off the SDP the peer emitted — three processes, and the
	//     only way to know the chain holds is to read the answer back from the
	//     dashboard after a real patron has joined.
	adminBase := fmt.Sprintf("http://127.0.0.1:%d", adminPort)
	scope := waitForScope(t, adminBase, 45*time.Second, func(sc model.ICEScopeReport) bool {
		return sc.Answered
	})
	if scope.OfferedCandidates == 0 {
		t.Fatal("patrons have been receiving Opus and the appliance reports offering them no candidates at all")
	}
	// Unconfigured here (this appliance never ran the setup wizard), so the
	// honest report is "not confined" — and it must say so rather than let the
	// mechanism's existence read as confinement.
	if scope.Configured || scope.InForce {
		t.Fatalf("no patron interface was ever configured, yet the dashboard reports %+v", scope)
	}

	// Now configure one, exactly as the setup wizard does, and require that the
	// next patron's answer is confined to it. This is the whole slice end to end:
	// a setting in core's SQLite store changes which addresses a phone is handed
	// by a different process.
	patronIface := firstRoutableIface(t)
	setupNetwork(t, adminBase, patronIface)
	confined := waitForScope(t, adminBase, 60*time.Second, func(sc model.ICEScopeReport) bool {
		return sc.InForce && sc.Interface == patronIface
	})
	if !confined.InForce {
		t.Fatalf("the configured patron interface never reached the media peer: %+v", confined)
	}
	late := joinAsPatron(t, ctx, patronPort, model.StreamHI)
	late.waitForOpus(t, 10)
	// The dashboard's copy of this measurement is refreshed by core's posture
	// reconciliation, not by the join, so it is at most one reconcile interval
	// behind the answer it describes — the same freshness every other posture row
	// on that page has. Allow several intervals rather than assuming the first
	// read is the new one: the failure this guards against is a CONFINED
	// appliance, and reading the pre-configuration measurement would report it as
	// unconfined.
	after4b := waitForScope(t, adminBase, 90*time.Second, func(sc model.ICEScopeReport) bool {
		return len(sc.OfferedInterfaces) == 1 && sc.OfferedInterfaces[0] == patronIface
	})
	if len(after4b.OfferedInterfaces) != 1 || after4b.OfferedInterfaces[0] != patronIface {
		t.Fatalf("with %q configured as the patron interface, the last patron was offered %v",
			patronIface, after4b.OfferedInterfaces)
	}
	late.close()

	// 5. And stopping the streams must take the encoders with them rather than
	//    orphaning a gst-launch child per named stream for the life of the host.
	first.close()
	second.close()
	_ = media.Process.Signal(os.Interrupt)
	waitUntil(t, 15*time.Second, func() bool {
		for _, pid := range afterReloads {
			if processAlive(pid) {
				return false
			}
		}
		return true
	})
	for _, pid := range afterReloads {
		if processAlive(pid) {
			t.Fatalf("encoder child %s outlived the media process: a stopped stream orphaned its encoder", pid)
		}
	}
}

// subscriber is one patron: a control socket and a receive-only peer, speaking
// exactly the protocol internal/web/assets/patron.js speaks.
type subscriber struct {
	conn    *websocket.Conn
	pc      *webrtc.PeerConnection
	packets <-chan *rtpPacket
	codec   <-chan string
	count   atomic.Int64
	done    chan struct{}
	once    sync.Once
}

func joinAsPatron(t *testing.T, ctx context.Context, patronPort int, stream model.StreamType) *subscriber {
	t.Helper()
	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/api/audio/ws", patronPort)
	conn, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("patron audio socket: %v", err)
	}
	if f := readFrame(t, ctx, conn); f["type"] != "audio_state" {
		t.Fatalf("first frame on the audio socket = %v, want audio_state", f["type"])
	}
	pc, packets, codec := patronPeer(t)
	sub := &subscriber{conn: conn, pc: pc, packets: packets, codec: codec, done: make(chan struct{})}
	t.Cleanup(sub.close)

	sendFrame(t, ctx, conn, web.SignalFrame{Type: "select", Stream: stream})
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
			Type: "ice", Stream: stream, Candidate: j.Candidate,
			SDPMid: mid, SDPMLineIndex: idx,
		})
	})
	sendFrame(t, ctx, conn, web.SignalFrame{Type: "offer", Stream: stream, SDP: offer.SDP})

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
	// Drain continuously so a subscriber left running in the background keeps a
	// live count instead of filling its channel and looking dead.
	go func() {
		for {
			select {
			case <-sub.done:
				return
			case p := <-packets:
				if len(p.Payload) == 0 {
					continue
				}
				sub.count.Add(1)
			}
		}
	}()
	return sub
}

func (s *subscriber) received() int64 { return s.count.Load() }

// waitForOpus blocks until n RTP packets carrying an Opus payload have arrived,
// and checks the track's codec is really Opus rather than whatever the SDP said.
func (s *subscriber) waitForOpus(t *testing.T, n int64) int64 {
	t.Helper()
	waitUntil(t, 30*time.Second, func() bool { return s.count.Load() >= n })
	if s.count.Load() < n {
		t.Fatalf("only %d RTP packets reached the subscriber: the end-to-end path is not delivering audio", s.count.Load())
	}
	select {
	case mime := <-s.codec:
		if !strings.EqualFold(mime, webrtc.MimeTypeOpus) {
			t.Fatalf("the track a patron received is %q, not Opus", mime)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no track ever started on this subscriber")
	}
	return s.count.Load()
}

// close is the patron closing the page: the control socket goes, which is what
// releases the appliance-side peer (internal/web/audio.go releasePeer).
func (s *subscriber) close() {
	s.once.Do(func() {
		close(s.done)
		_ = s.conn.Close(websocket.StatusNormalClosure, "")
		_ = s.pc.Close()
	})
}

func waitUntil(t *testing.T, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// logTail collects a process's JSON log stream so a test can assert on the
// EVENTS it emitted. It reads counts and event names only — never a body.
type logTail struct {
	mu  sync.Mutex
	buf []byte
}

func (l *logTail) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	return len(p), nil
}

// peakListeners is the highest connected-listener count the media process ever
// reported on a webrtc.peer.connected event (§6.6).
func (l *logTail) peakListeners(t *testing.T) int {
	t.Helper()
	l.mu.Lock()
	lines := strings.Split(string(l.buf), "\n")
	l.mu.Unlock()
	peak := 0
	seen := false
	for _, line := range lines {
		var rec struct {
			Event     string `json:"event"`
			Listeners *int   `json:"listeners"`
		}
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		if rec.Event != string(logging.EvWebRTCPeerConnected) || rec.Listeners == nil {
			continue
		}
		seen = true
		if *rec.Listeners > peak {
			peak = *rec.Listeners
		}
	}
	if !seen {
		t.Fatal("the media process never logged a webrtc.peer.connected event: no peer ever reached the connected state")
	}
	return peak
}

// processAlive reports whether a pid is still a live process (not a zombie).
func processAlive(pid string) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return false
	}
	fields := strings.Fields(string(b))
	// state is the field after the (comm) parenthesis; a reaped-but-unwaited
	// child shows Z and is not running anything.
	for i, f := range fields {
		if strings.HasSuffix(f, ")") && i+1 < len(fields) {
			return fields[i+1] != "Z"
		}
	}
	return true
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

func start(t *testing.T, bin string, args ...string) *exec.Cmd {
	return startCapturing(t, nil, bin, args...)
}

// startCapturing runs one appliance process, optionally tee-ing its log stream
// into sink as well as the test's own output.
func startCapturing(t *testing.T, sink io.Writer, bin string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var out io.Writer = os.Stderr
	if sink != nil {
		out = io.MultiWriter(os.Stderr, sink)
	}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting %s: %v", filepath.Base(bin), err)
	}
	t.Cleanup(func() {
		// By PID: `pkill -x openaccessbridge-core` matches nothing, because Linux
		// truncates a process name to 15 characters.
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return cmd
}

// encoderChildren counts the encoder subprocesses openaccessbridge-media is running.
// §6.7's topology is a per-STREAM property, so this number must track the number
// of named streams being encoded and must not move when subscribers come and go.
func encoderChildren(t *testing.T, mediaPID int) []string {
	t.Helper()
	out, err := exec.Command("pgrep", "-P", strconv.Itoa(mediaPID)).Output()
	if err != nil {
		// pgrep exits 1 when there is no match, which is a legitimate zero.
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil
		}
		t.Skipf("cannot enumerate the media process's children on this host: %v", err)
	}
	var kids []string
	for _, pid := range strings.Fields(string(out)) {
		// Only the encoder children count: the invariant under test is about
		// Opus encoders per named stream, not about every helper media might run.
		comm, cerr := os.ReadFile("/proc/" + pid + "/comm")
		if cerr != nil || !strings.HasPrefix(strings.TrimSpace(string(comm)), "gst-launch") {
			continue
		}
		kids = append(kids, pid)
	}
	return kids
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

// waitForScope polls the admin status until the ICE scope satisfies cond, and
// returns it. It reads the DASHBOARD rather than the media process directly:
// the point of the assertion is that the measurement survives all three hops.
func waitForScope(t *testing.T, adminBase string, within time.Duration, cond func(model.ICEScopeReport) bool) model.ICEScopeReport {
	t.Helper()
	deadline := time.Now().Add(within)
	var last model.ICEScopeReport
	for time.Now().Before(deadline) {
		resp, err := http.Get(adminBase + "/api/admin/status")
		if err == nil {
			var doc model.SystemReadiness
			derr := json.NewDecoder(resp.Body).Decode(&doc)
			_ = resp.Body.Close()
			if derr == nil {
				last = doc.ICEScope
				if cond(last) {
					return last
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the ICE scope never satisfied the condition within %s; last was %+v", within, last)
	return last
}

// setupNetwork records the patron interface through the operator's own API, so
// the test drives the same path a booth tech does rather than writing to the
// store behind the product's back.
func setupNetwork(t *testing.T, adminBase, clientIface string) {
	t.Helper()
	theater := "lo"
	if clientIface == theater {
		t.Skip("the only routable interface is the loopback stand-in")
	}
	body := fmt.Sprintf(`{"TheaterIface":%q,"ClientIface":%q}`, theater, clientIface)
	resp, err := http.Post(adminBase+"/api/admin/setup/network", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("configuring the patron interface: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("configuring the patron interface: %s: %s", resp.Status, b)
	}
}

// firstRoutableIface is the interface this test treats as the patron LAN.
func firstRoutableIface(t *testing.T) string {
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
			return i.Name
		}
	}
	t.Skip("no non-loopback interface to stand in for the patron LAN")
	return ""
}
