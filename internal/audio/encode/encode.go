// Package encode is the Opus-encode + WebRTC-transport seam for captionbridge-media
// (buildplan.md §6.7). The encoder/transport is pluggable so the qualifier and
// channel mapper (pure Go) stay testable without GStreamer, and so the media
// backend can be swapped (e.g. aiortc) without touching the caption core (§3.5).
//
// On this dev host the GStreamer RUNTIME (opusenc, audiotestsrc, level) is present
// but webrtcbin and the dev headers are NOT (DECISIONS D5, BLOCKERS B2/B4). So the
// GStreamer backend here drives a `gst-launch-1.0` Opus pipeline as a real local
// SMOKE TEST (synthetic tone → Opus), while the WebRTC peer is a documented seam
// to be completed when webrtcbin / a real phone fleet is available.
package encode

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// OpusParams captures the low-latency Opus tuning from §6.7.
type OpusParams struct {
	FrameMS     int    // 10 ms target (20 ms stability fallback)
	Application string // "lowdelay"/"restricted-lowdelay"/"voip"
	FEC         bool   // enabled for Wi-Fi resilience
	DTX         bool   // OFF for HI/VI-N (don't gate speech onsets)
	Bitrate     int    // bits/sec
}

// DefaultOpusParams returns the §6.7 low-latency defaults.
func DefaultOpusParams() OpusParams {
	return OpusParams{FrameMS: 10, Application: "restricted-lowdelay", FEC: true, DTX: false, Bitrate: 48000}
}

// Backend is an encode/transport implementation.
type Backend interface {
	Name() string
	// Available reports whether the backend can run on this host right now.
	Available() bool
	// SmokeTest exercises an end-to-end encode of a synthetic tone for dur and
	// returns nil on success. It proves the pipeline works locally without a real
	// device or phone.
	SmokeTest(ctx context.Context, dur time.Duration, p OpusParams) error
}

// GStreamerBackend runs the media pipeline via `gst-launch-1.0`.
type GStreamerBackend struct {
	// Bin is the gst-launch binary (default "gst-launch-1.0").
	Bin string
}

// NewGStreamerBackend builds a GStreamerBackend.
func NewGStreamerBackend() *GStreamerBackend { return &GStreamerBackend{Bin: "gst-launch-1.0"} }

// Name identifies the backend.
func (g *GStreamerBackend) Name() string { return "gstreamer(gst-launch)" }

// Available reports whether gst-launch-1.0 and the opusenc element are present.
func (g *GStreamerBackend) Available() bool {
	bin := g.bin()
	if _, err := exec.LookPath(bin); err != nil {
		return false
	}
	// Confirm the opusenc element exists via gst-inspect.
	inspect := strings.Replace(bin, "gst-launch", "gst-inspect", 1)
	if _, err := exec.LookPath(inspect); err != nil {
		return false
	}
	out, err := exec.Command(inspect, "opusenc").CombinedOutput()
	return err == nil && len(out) > 0
}

func (g *GStreamerBackend) bin() string {
	if g.Bin == "" {
		return "gst-launch-1.0"
	}
	return g.Bin
}

// SmokeTest runs: audiotestsrc → audioconvert → audioresample → opusenc → fakesink
// for dur, proving the capture/encode chain works locally (kickoff item 3). It uses
// a synthetic tone source so no hardware is required, and discards the output
// (fakesink) so nothing is persisted.
func (g *GStreamerBackend) SmokeTest(ctx context.Context, dur time.Duration, p OpusParams) error {
	if !g.Available() {
		return fmt.Errorf("gstreamer opus pipeline unavailable on this host")
	}
	sampleRate := 48000
	frameSamples := sampleRate * maxInt(p.FrameMS, 10) / 1000
	numBuffers := int(dur.Milliseconds()) / maxInt(p.FrameMS, 10)
	if numBuffers < 1 {
		numBuffers = 1
	}
	dtx := boolToTF(p.DTX)
	fec := boolToTF(p.FEC)
	pipeline := []string{
		"-q",
		"audiotestsrc", fmt.Sprintf("num-buffers=%d", numBuffers), "wave=sine", "freq=440",
		fmt.Sprintf("samplesperbuffer=%d", frameSamples), "!",
		"audioconvert", "!", "audioresample", "!",
		fmt.Sprintf("audio/x-raw,rate=%d,channels=1", sampleRate), "!",
		"opusenc", fmt.Sprintf("bitrate=%d", maxInt(p.Bitrate, 32000)),
		fmt.Sprintf("frame-size=%d", maxInt(p.FrameMS, 10)),
		fmt.Sprintf("dtx=%s", dtx), fmt.Sprintf("inband-fec=%s", fec), "!",
		"fakesink", "sync=false",
	}
	cmd := exec.CommandContext(ctx, g.bin(), pipeline...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gst opus smoke pipeline failed: %v: %s", err, truncate(string(out), 240))
	}
	return nil
}

// NullBackend is a no-op backend used where media is disabled or in unit tests.
type NullBackend struct{}

func (NullBackend) Name() string    { return "null" }
func (NullBackend) Available() bool { return true }
func (NullBackend) SmokeTest(context.Context, time.Duration, OpusParams) error {
	return nil
}

// Select returns the best available backend: GStreamer if present, else Null.
func Select() Backend {
	g := NewGStreamerBackend()
	if g.Available() {
		return g
	}
	return NullBackend{}
}

func boolToTF(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
