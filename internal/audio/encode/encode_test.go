package encode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestNullBackendEncodesNothing locks in the honesty rule: the no-op stand-in
// must REPORT that nothing was encoded rather than returning a success the
// dashboard would render as a working media path.
func TestNullBackendEncodesNothing(t *testing.T) {
	var b Backend = NullBackend{}
	if !b.Available() {
		t.Fatal("null backend should be selectable")
	}
	err := b.SmokeTest(context.Background(), 50*time.Millisecond, DefaultOpusParams())
	if !errors.Is(err, ErrNoEncoder) {
		t.Fatalf("null smoke err = %v, want ErrNoEncoder", err)
	}
}

func TestSelectReturnsBackend(t *testing.T) {
	if Select() == nil {
		t.Fatal("Select returned nil")
	}
}

// TestOpusAudioTypeMapping asserts §6.7's application/audio-type tuning is
// actually mapped onto the encoder element (it was declared and never used), and
// that an unknown value is rejected instead of silently falling back to the
// element default.
func TestOpusAudioTypeMapping(t *testing.T) {
	tests := []struct {
		name    string
		app     string
		want    string
		wantErr bool
	}{
		{name: "default is restricted-lowdelay", app: DefaultOpusParams().Application, want: "restricted-lowdelay"},
		{name: "spec spelling lowdelay maps to restricted-lowdelay", app: "lowdelay", want: "restricted-lowdelay"},
		{name: "underscore spelling", app: "restricted_lowdelay", want: "restricted-lowdelay"},
		{name: "empty falls back to the low-delay default", app: "", want: "restricted-lowdelay"},
		{name: "voip is the voice mode", app: "voip", want: "voice"},
		{name: "voice", app: "VOICE", want: "voice"},
		{name: "audio is generic", app: "audio", want: "generic"},
		{name: "generic", app: " generic ", want: "generic"},
		{name: "unknown is rejected", app: "hifi", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := opusAudioType(tc.app)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("opusAudioType(%q) = %q, want an error", tc.app, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("opusAudioType(%q): %v", tc.app, err)
			}
			if got != tc.want {
				t.Errorf("opusAudioType(%q) = %q, want %q", tc.app, got, tc.want)
			}
		})
	}
}

// TestSmokeTestRejectsUnknownApplication proves the rejection reaches the
// backend: a bad application must fail before gst-launch is invoked.
func TestSmokeTestRejectsUnknownApplication(t *testing.T) {
	g := NewGStreamerBackend()
	if !g.Available() {
		t.Skip("gst-launch-1.0 / opusenc not available on this host")
	}
	p := DefaultOpusParams()
	p.Application = "hifi"
	err := g.SmokeTest(context.Background(), 50*time.Millisecond, p)
	if err == nil || !strings.Contains(err.Error(), "unknown Opus application") {
		t.Fatalf("SmokeTest err = %v, want an unknown-application error", err)
	}
}

// TestGStreamerOpusSmoke runs a real synthetic-tone → Opus pipeline if the
// GStreamer runtime is present on this host; otherwise it skips honestly (the
// pipeline cannot be exercised without gst-launch + opusenc). This proves the
// capture/encode chain works locally without any hardware (kickoff item 3), and
// that opusenc accepts every §6.7 audio-type this build can emit.
func TestGStreamerOpusSmoke(t *testing.T) {
	g := NewGStreamerBackend()
	if !g.Available() {
		t.Skip("gst-launch-1.0 / opusenc not available on this host; pipeline smoke test skipped")
	}
	for _, app := range []string{"restricted-lowdelay", "voip", "audio"} {
		t.Run(app, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			p := DefaultOpusParams()
			p.Application = app
			if err := g.SmokeTest(ctx, 200*time.Millisecond, p); err != nil {
				t.Fatalf("gstreamer opus smoke test (%s) failed: %v", app, err)
			}
		})
	}
}
