package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/audio/encode"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

func newManager(t *testing.T, synthetic bool) *manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := newManagerFor(ctx, logging.Nop(), encode.Select(), "", synthetic)
	t.Cleanup(func() {
		m.stopAll()
		cancel()
	})
	return m
}

// TestConfigureAppliesTheStoredChannelMapping: §10.5's channel_mapping is
// configuration the media process CONSUMES. A stored "stereo:L=HI,R=VI-N" must
// produce an HI stream fed from the left channel and a VI-N stream fed from the
// right — not two hard-coded mono streams.
func TestConfigureAppliesTheStoredChannelMapping(t *testing.T) {
	m := newManager(t, true)
	res := m.configure(model.AuditoriumProfile{
		AuditoriumID: "screen-3", ChannelMapping: "stereo:L=HI,R=VI-N",
	})
	if !res.OK {
		t.Fatalf("configure refused a valid mapping: %s", res.Error)
	}
	if len(res.Streams) != 2 {
		t.Fatalf("expected two running streams, got %d", len(res.Streams))
	}
	hi, vin := m.slots[model.StreamHI], m.slots[model.StreamVIN]
	if hi == nil || vin == nil {
		t.Fatalf("both named streams must be configured, got %v", m.slots)
	}
	if hi.cfg.Source != model.ChannelLeft {
		t.Errorf("HI source = %q, want left", hi.cfg.Source)
	}
	if vin.cfg.Source != model.ChannelRight {
		t.Errorf("VI-N source = %q, want right", vin.cfg.Source)
	}
	if hi.channels != 2 {
		t.Errorf("a stereo split needs a 2-channel input, got %d", hi.channels)
	}
	// With no device chosen yet, the streams run under the stand-in's OWN name.
	// Labelling them with a booth device's identity is what would turn a tone
	// into a fabricated measurement of that device (see the test below).
	if !strings.HasPrefix(hi.cfg.DeviceID, syntheticDevicePrefix) {
		t.Errorf("device id = %q, want the synthetic stand-in's own name", hi.cfg.DeviceID)
	}
}

// TestConfigureHonorsTheOtherMappings covers the §6.5 table end to end.
func TestConfigureHonorsTheOtherMappings(t *testing.T) {
	cases := []struct {
		mapping string
		stream  model.StreamType
		source  model.SourceChannel
		streams int
	}{
		{"mono:HI", model.StreamHI, model.ChannelMono, 1},
		{"summed:VI-N", model.StreamVIN, model.ChannelSummed, 1},
		{"stereo-pair:HI", model.StreamHI, model.ChannelStereo, 1},
		{"stereo:L=VI-N,R=HI", model.StreamVIN, model.ChannelLeft, 2},
	}
	for _, tc := range cases {
		m := newManager(t, true)
		res := m.configure(model.AuditoriumProfile{ChannelMapping: tc.mapping})
		if !res.OK {
			t.Fatalf("%s: refused: %s", tc.mapping, res.Error)
		}
		if len(res.Streams) != tc.streams {
			t.Errorf("%s: %d streams, want %d", tc.mapping, len(res.Streams), tc.streams)
		}
		s := m.slots[tc.stream]
		if s == nil || s.cfg.Source != tc.source {
			t.Errorf("%s: %s source = %v, want %q", tc.mapping, tc.stream, s, tc.source)
		}
	}
}

// TestConfigureRefusesAnUnrecognizedMapping: a mapping nobody can honor must be
// refused with a named reason, not left running the previous plan (guardrail #7).
func TestConfigureRefusesAnUnrecognizedMapping(t *testing.T) {
	m := newManager(t, true)
	m.applyDefaultSynthetic()
	res := m.configure(model.AuditoriumProfile{ChannelMapping: "quadraphonic:L=HI"})
	if res.OK {
		t.Fatalf("an unrecognized mapping must be refused")
	}
	if !strings.Contains(res.Error, "channel mapping") {
		t.Errorf("the refusal must name the cause, got %q", res.Error)
	}
}

// TestConfigureRefusesARealDeviceRatherThanFakeIt: there is no ALSA capture
// source yet (B3/B4). Serving a booth's configured input with a synthetic tone
// would be a fabricated measurement, so the process refuses and lets core shout.
//
// The gate is the PROFILE's device, not the --synthetic flag: with the flag on
// (its default, and what the systemd unit runs) a stored input_device_id would
// otherwise label a tone with the booth's hardware identity, and core persists
// that as §10.5 last_qualification and §10.8 audio_device_compat_profile — a
// verdict on hardware nothing ever opened.
func TestConfigureRefusesARealDeviceRatherThanFakeIt(t *testing.T) {
	for _, synthetic := range []bool{true, false} {
		m := newManager(t, synthetic)
		m.applyDefaultSynthetic()
		res := m.configure(model.AuditoriumProfile{
			InputDeviceID: "usb-Focusrite_Scarlett_2i2-00", ChannelMapping: "mono:HI",
		})
		if res.OK {
			t.Fatalf("synthetic=%v: a real capture device must not be served by a synthetic source", synthetic)
		}
		if !strings.Contains(res.Error, "B3") {
			t.Errorf("synthetic=%v: the refusal should name the blocker, got %q", synthetic, res.Error)
		}
		// And no stream may end up wearing that device's identity.
		for _, st := range m.states() {
			if st.InputDeviceID == "usb-Focusrite_Scarlett_2i2-00" {
				t.Errorf("synthetic=%v: a stream is reporting itself as the real device", synthetic)
			}
		}
	}
}

// TestNoSourceAtAllIsRefused: --synthetic=false leaves this build with no
// capture.Source whatsoever, so even a device-less profile is refused.
func TestNoSourceAtAllIsRefused(t *testing.T) {
	m := newManager(t, false)
	res := m.configure(model.AuditoriumProfile{ChannelMapping: "mono:HI"})
	if res.OK {
		t.Fatalf("with no capture backend at all, configure must refuse")
	}
	if len(m.slots) != 0 {
		t.Errorf("a refused configure must not install a plan, got %v", m.slots)
	}
}

// TestStopAndStartOneStream: §3.5's media.start/media.stop act on one named
// stream, leaving the other running — HI and VI-N are independent (§6.1).
func TestStopAndStartOneStream(t *testing.T) {
	m := newManager(t, true)
	if res := m.configure(model.AuditoriumProfile{ChannelMapping: "stereo:L=HI,R=VI-N"}); !res.OK {
		t.Fatalf("configure: %s", res.Error)
	}
	res := m.stop(model.StreamVIN)
	if !res.OK {
		t.Fatalf("stop: %s", res.Error)
	}
	if len(res.Streams) != 1 {
		t.Errorf("a stopped stream is not reported as a live input, got %d", len(res.Streams))
	}
	if m.slots[model.StreamVIN].stream != nil {
		t.Errorf("VI-N should not be running")
	}
	if m.slots[model.StreamHI].stream == nil {
		t.Errorf("stopping VI-N must not stop HI")
	}
	if res := m.start(model.StreamVIN); !res.OK || len(res.Streams) != 2 {
		t.Errorf("start should bring it back: ok=%v streams=%d err=%q", res.OK, len(res.Streams), res.Error)
	}
}

// TestStartRefusesAStreamOutsideTheMapping.
func TestStartRefusesAStreamOutsideTheMapping(t *testing.T) {
	m := newManager(t, true)
	if res := m.configure(model.AuditoriumProfile{ChannelMapping: "mono:HI"}); !res.OK {
		t.Fatalf("configure: %s", res.Error)
	}
	if res := m.start(model.StreamVIN); res.OK {
		t.Fatalf("VI-N is not in a mono:HI mapping; starting it must be refused")
	}
}

// TestIPCVerbsAreImplemented: media.configure/start/stop were declared IPC types
// with no handler — every one of them must now do something over the wire (§3.5).
func TestIPCVerbsAreImplemented(t *testing.T) {
	m := newManager(t, true)
	prof, _ := json.Marshal(model.AuditoriumProfile{ChannelMapping: "stereo:L=HI,R=VI-N"})
	name, _ := json.Marshal(model.StreamHI)

	for _, tc := range []struct {
		verb    string
		payload []byte
	}{
		{ipc.TypeMediaConfigure, prof},
		{ipc.TypeMediaStop, name},
		{ipc.TypeMediaStart, name},
	} {
		resp := m.handle(ipc.Message{Type: tc.verb, ID: "1", Payload: tc.payload})
		if resp == nil || resp.Type == "unsupported" {
			t.Fatalf("%s: not handled", tc.verb)
		}
		var res model.MediaCommandResult
		if err := json.Unmarshal(resp.Payload, &res); err != nil {
			t.Fatalf("%s: bad reply: %v", tc.verb, err)
		}
		if !res.OK {
			t.Errorf("%s: %s", tc.verb, res.Error)
		}
	}
	// media.status keeps its own shape (a bare array of stream states).
	resp := m.handle(ipc.Message{Type: ipc.TypeMediaStatus, ID: "2"})
	var states []model.AudioStreamState
	if err := json.Unmarshal(resp.Payload, &states); err != nil {
		t.Fatalf("media.status shape changed: %v", err)
	}
	if len(states) != 2 {
		t.Errorf("expected two live streams, got %d", len(states))
	}
}

// TestMalformedPayloadsAreRefused: a garbled command must not be silently
// ignored or crash the media process.
func TestMalformedPayloadsAreRefused(t *testing.T) {
	m := newManager(t, true)
	for _, verb := range []string{ipc.TypeMediaConfigure, ipc.TypeMediaStart, ipc.TypeMediaStop} {
		resp := m.handle(ipc.Message{Type: verb, ID: "1", Payload: []byte(`{"not":`)})
		var res model.MediaCommandResult
		_ = json.Unmarshal(resp.Payload, &res)
		if res.OK || res.Error == "" {
			t.Errorf("%s: a malformed payload must be refused with a reason, got %+v", verb, res)
		}
	}
}
