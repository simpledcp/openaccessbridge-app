package main

import (
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// §6.5's "Multiple USB inputs: Device A = HI, Device B = VI-N". The mapper has
// always implemented it; before the profile could hold a per-stream device, a
// room wired that way could not be configured at all.
func TestATwoDeviceRoomConfiguresWithADeviceBehindEachStream(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	res := m.configure(model.AuditoriumProfile{
		ChannelMapping: "multi-device",
		StreamDevices: map[model.StreamType]string{
			model.StreamHI:  syntheticDevicePrefix + "hi-dongle",
			model.StreamVIN: syntheticDevicePrefix + "vin-dongle",
		},
	})
	if !res.OK {
		t.Fatalf("a valid two-device room was refused: %s", res.Error)
	}
	// Each stream must be attributed to the device it is really fed from: a
	// qualifier verdict recorded against the wrong device sends a tech to the
	// wrong cable.
	states := m.states()
	if len(states) != 2 {
		t.Fatalf("expected two streams, got %d", len(states))
	}
	byStream := map[model.StreamType]string{}
	for _, st := range states {
		byStream[st.StreamType] = st.InputDeviceID
	}
	if !strings.Contains(byStream[model.StreamHI], "hi-dongle") {
		t.Errorf("HI is attributed to %q, not its own device", byStream[model.StreamHI])
	}
	if !strings.Contains(byStream[model.StreamVIN], "vin-dongle") {
		t.Errorf("VI-N is attributed to %q, not its own device", byStream[model.StreamVIN])
	}
}

// The half-real pairing: one synthetic device and one real one. Checking only the
// first would have served a tone under the real device's identity for the other
// stream — the same fabrication the single-device refusal exists to prevent.
func TestAPairingWithOneRealDeviceIsRefusedForThatDevice(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	res := m.configure(model.AuditoriumProfile{
		ChannelMapping: "multi-device",
		StreamDevices: map[model.StreamType]string{
			model.StreamHI:  syntheticDevicePrefix + "hi-dongle",
			model.StreamVIN: "usb-audio-real-dongle",
		},
	})
	if res.OK {
		t.Fatal("a pairing naming a real capture device was accepted (BLOCKERS B3/B4)")
	}
	if !strings.Contains(res.Error, "usb-audio-real-dongle") {
		t.Fatalf("the refusal does not name the device it cannot open: %s", res.Error)
	}
	if !strings.Contains(res.Error, string(model.StreamVIN)) {
		t.Fatalf("the refusal does not say which stream that device feeds: %s", res.Error)
	}
}

// A stream with no device feeds nothing, and §6.5 says a mapping the hardware
// cannot satisfy surfaces BEFORE a show rather than as a silent fault during one.
func TestAnIncompletePairingIsRefusedBeforeAnythingStarts(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	res := m.configure(model.AuditoriumProfile{
		ChannelMapping: "multi-device",
		StreamDevices:  map[model.StreamType]string{model.StreamHI: syntheticDevicePrefix + "hi"},
	})
	if res.OK {
		t.Fatal("a pairing with nothing feeding VI-N was accepted")
	}
	if len(m.states()) != 0 {
		t.Fatal("streams were started from a configuration that was refused")
	}
}

// Two streams on one device is a channel mapping recorded as a pairing, and it
// hides which channel each stream really takes.
func TestBothStreamsOnOneDeviceIsRefusedByTheMediaProcess(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	res := m.configure(model.AuditoriumProfile{
		ChannelMapping: "multi-device",
		StreamDevices: map[model.StreamType]string{
			model.StreamHI:  syntheticDevicePrefix + "one",
			model.StreamVIN: syntheticDevicePrefix + "one",
		},
	})
	if res.OK {
		t.Fatal("both streams on one device passed as a multi-device pairing")
	}
}
