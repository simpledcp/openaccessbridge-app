package encode

import (
	"context"
	"testing"
	"time"
)

func TestNullBackend(t *testing.T) {
	var b Backend = NullBackend{}
	if !b.Available() {
		t.Fatal("null backend should be available")
	}
	if err := b.SmokeTest(context.Background(), 50*time.Millisecond, DefaultOpusParams()); err != nil {
		t.Fatalf("null smoke: %v", err)
	}
}

func TestSelectReturnsBackend(t *testing.T) {
	if Select() == nil {
		t.Fatal("Select returned nil")
	}
}

// TestGStreamerOpusSmoke runs a real synthetic-tone → Opus pipeline if the
// GStreamer runtime is present on this host; otherwise it skips honestly (the
// pipeline cannot be exercised without gst-launch + opusenc). This proves the
// capture/encode chain works locally without any hardware (kickoff item 3).
func TestGStreamerOpusSmoke(t *testing.T) {
	g := NewGStreamerBackend()
	if !g.Available() {
		t.Skip("gst-launch-1.0 / opusenc not available on this host; pipeline smoke test skipped")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := g.SmokeTest(ctx, 200*time.Millisecond, DefaultOpusParams()); err != nil {
		t.Fatalf("gstreamer opus smoke test failed: %v", err)
	}
}
