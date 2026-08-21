package model

import "testing"

// §6.5 lists "Multiple USB inputs: Device A = HI, Device B = VI-N" as a supported
// mapping. Before this the profile had one device field, so a room wired that way
// could not be configured at all — the wizard offered the mode and then refused
// to save the pairing.
func TestATwoDeviceRoomIsRepresentable(t *testing.T) {
	p := AuditoriumProfile{
		ChannelMapping: "multi-device",
		InputDeviceID:  "usb-dongle-a",
		StreamDevices:  map[StreamType]string{StreamHI: "usb-dongle-a", StreamVIN: "usb-dongle-b"},
	}
	if err := p.ValidateInputs(true, []StreamType{StreamHI, StreamVIN}); err != nil {
		t.Fatalf("a correctly paired two-device room was refused: %v", err)
	}
	if dev, ok := p.InputDeviceFor(StreamHI); !ok || dev != "usb-dongle-a" {
		t.Fatalf("HI resolves to %q (ok=%v)", dev, ok)
	}
	if dev, ok := p.InputDeviceFor(StreamVIN); !ok || dev != "usb-dongle-b" {
		t.Fatalf("VI-N resolves to %q (ok=%v)", dev, ok)
	}
}

// A stream with no device feeds nothing, and a patron who selects that feed hears
// silence. That must be caught before a show, not during one.
func TestAStreamWithNoDeviceIsRefused(t *testing.T) {
	p := AuditoriumProfile{
		ChannelMapping: "multi-device",
		StreamDevices:  map[StreamType]string{StreamHI: "usb-dongle-a"},
	}
	err := p.ValidateInputs(true, []StreamType{StreamHI, StreamVIN})
	if err == nil {
		t.Fatal("a room with VI-N assigned to nothing was accepted")
	}
	if !contains(err.Error(), "VI-N") {
		t.Fatalf("the refusal does not name the stream with no device: %v", err)
	}
}

// Two streams on one device is a CHANNEL MAPPING, not a multi-device pairing.
// Recording it as one hides which channel each stream really takes.
func TestBothStreamsOnOneDeviceIsRefused(t *testing.T) {
	p := AuditoriumProfile{
		ChannelMapping: "multi-device",
		StreamDevices:  map[StreamType]string{StreamHI: "usb-dongle-a", StreamVIN: "usb-dongle-a"},
	}
	if err := p.ValidateInputs(true, []StreamType{StreamHI, StreamVIN}); err == nil {
		t.Fatal("both streams on one device passed as a multi-device pairing")
	}
}

// Per-stream devices on a single-device mapping would be silently ignored, and
// the room would run on a mapping nobody chose.
func TestPerStreamDevicesOnASingleDeviceMappingAreRefused(t *testing.T) {
	p := AuditoriumProfile{
		ChannelMapping: "stereo:L=HI,R=VI-N",
		InputDeviceID:  "usb-dongle-a",
		StreamDevices:  map[StreamType]string{StreamHI: "usb-dongle-a", StreamVIN: "usb-dongle-b"},
	}
	if err := p.ValidateInputs(false, []StreamType{StreamHI, StreamVIN}); err == nil {
		t.Fatal("a stereo mapping carrying per-stream devices was accepted")
	}
}

// An empty single-device InputDeviceID is NOT judged here. On this build it is a
// real state — no device chosen, so the synthetic stand-in feeds the streams —
// and this type cannot tell that apart from a half-finished configuration. The
// caller that knows the difference decides (cmd/captionbridge-media does, and
// refuses any device it has no backend for).
func TestAnEmptySingleDeviceIsLeftToTheCaller(t *testing.T) {
	p := AuditoriumProfile{ChannelMapping: "mono:HI"}
	if err := p.ValidateInputs(false, []StreamType{StreamHI}); err != nil {
		t.Fatalf("the model judged a state it cannot interpret: %v", err)
	}
	if _, ok := p.InputDeviceFor(StreamHI); ok {
		t.Fatal("a profile with no device reported one")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
