package capture

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- fixture tree helpers -------------------------------------------------
//
// The enumerator is exercised against a fixture directory rather than this
// host's sound cards, so the test asserts the same behavior on any machine and
// on a CI box with no audio hardware at all.

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func symlink(t *testing.T, dir, name, target string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatalf("symlink %s: %v", name, err)
	}
}

func pcmInfoFile(card, device int, stream, name string) string {
	return "card: " + itoa(card) + "\ndevice: " + itoa(device) + "\nsubdevice: 0\nstream: " + stream +
		"\nid: " + name + "\nname: " + name + "\nsubdevices_count: 1\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// usbStream0 is the snd-usb-audio descriptor format (/proc/asound/cardN/stream0),
// the only place the kernel publishes a USB device's channel count without the
// device being opened.
//
// The PLAYBACK section deliberately advertises MORE channels than the capture
// section, and the capture section deliberately lists two altsettings: an input
// device must be reported by what it can CAPTURE (the widest capture altsetting,
// 2), never by an output width it happens to offer (8). §6.1 detects the
// "per-input channel count", and a wrong count would make a legal stereo mapping
// look satisfiable on a mono input, or the reverse (§6.5).
const usbStream0 = `USB Audio Device at usb-0000:00:14.0-1, high speed : USB Audio

Playback:
  Status: Stop
  Interface 1
    Altset 1
    Format: S24_3LE
    Channels: 8
    Endpoint: 0x01 (1 OUT) (ADAPTIVE)
    Rates: 48000, 44100

Capture:
  Status: Stop
  Interface 2
    Altset 1
    Format: S16_LE
    Channels: 1
    Endpoint: 0x82 (2 IN) (ASYNC)
    Rates: 48000, 44100
    Altset 2
    Format: S24_3LE
    Channels: 2
    Endpoint: 0x82 (2 IN) (ASYNC)
    Rates: 48000, 44100
`

// buildFixture lays out a three-card tree: an on-board HDA card reachable only
// by-path, a USB interface with a by-id link and a stream descriptor, and a card
// with no /dev/snd link at all.
func buildFixture(t *testing.T) (proc, dev string) {
	t.Helper()
	root := t.TempDir()
	proc, dev = filepath.Join(root, "proc"), filepath.Join(root, "dev")

	writeFile(t, filepath.Join(proc, "cards"), ` 0 [PCH            ]: HDA-Intel - HDA Intel PCH
                      HDA Intel PCH at 0xfb130000 irq 57
 1 [Solo           ]: USB-Audio - Scarlett Solo USB
                      Focusrite Scarlett Solo USB at usb-0000:00:14.0-1
 2 [Orphan         ]: HDA-Intel - Orphan Codec
                      Orphan Codec at 0xfb000000 irq 12
`)

	// Card 0: on-board codec, one capture PCM plus a playback PCM that must be
	// ignored, and no stream descriptor (channel count is not detectable).
	writeFile(t, filepath.Join(proc, "card0", "id"), "PCH\n")
	writeFile(t, filepath.Join(proc, "card0", "pcm0c", "info"), pcmInfoFile(0, 0, "CAPTURE", "ALC1150 Analog"))
	writeFile(t, filepath.Join(proc, "card0", "pcm0p", "info"), pcmInfoFile(0, 0, "PLAYBACK", "ALC1150 Analog"))
	writeFile(t, filepath.Join(proc, "card0", "pcm2c", "info"), pcmInfoFile(0, 2, "CAPTURE", "ALC1150 Alt Analog"))

	// Card 1: USB interface — by-id link and a stream descriptor.
	writeFile(t, filepath.Join(proc, "card1", "id"), "Solo\n")
	writeFile(t, filepath.Join(proc, "card1", "stream0"), usbStream0)
	writeFile(t, filepath.Join(proc, "card1", "pcm0c", "info"), pcmInfoFile(1, 0, "CAPTURE", "Scarlett Solo USB"))

	// Card 2: no /dev/snd link of either kind.
	writeFile(t, filepath.Join(proc, "card2", "id"), "Orphan\n")
	writeFile(t, filepath.Join(proc, "card2", "pcm0c", "info"), pcmInfoFile(2, 0, "CAPTURE", "Orphan Analog"))

	symlink(t, filepath.Join(dev, "by-id"), "usb-Focusrite_Scarlett_Solo_USB-00", "../controlC1")
	symlink(t, filepath.Join(dev, "by-path"), "pci-0000:00:1b.0", "../controlC0")
	symlink(t, filepath.Join(dev, "by-path"), "pci-0000:00:14.0-usb-0:1:1.0", "../controlC1")
	return proc, dev
}

// TestEnumerateFixture asserts the §6.1 identifier rules on a fixture tree:
// by-id wins, by-path is the fallback, a card index is NEVER identity, playback
// PCMs are not offered as inputs, and an undetectable channel count is reported
// as unknown rather than guessed.
func TestEnumerateFixture(t *testing.T) {
	proc, dev := buildFixture(t)
	devs, err := (&ALSAEnumerator{ProcRoot: proc, DevRoot: dev}).Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if len(devs) != 4 {
		t.Fatalf("got %d devices, want 4: %+v", len(devs), devs)
	}

	byName := map[string]Device{}
	for _, d := range devs {
		byName[d.FriendlyName] = d
	}

	tests := []struct {
		name         string
		friendly     string
		wantID       string
		wantKind     DeviceIDKind
		wantStable   bool
		wantChannels int
		wantALSA     string
	}{
		{
			name:     "usb interface prefers its by-id link",
			friendly: "Scarlett Solo USB",
			wantID:   "by-id:usb-Focusrite_Scarlett_Solo_USB-00,0",
			wantKind: DeviceIDByID, wantStable: true,
			// 2, not the 8 the PLAYBACK section advertises.
			wantChannels: 2, wantALSA: "hw:1,0",
		},
		{
			name:     "onboard codec falls back to by-path",
			friendly: "HDA Intel PCH — ALC1150 Analog",
			wantID:   "by-path:pci-0000:00:1b.0,0",
			wantKind: DeviceIDByPath, wantStable: true,
			wantChannels: 0, wantALSA: "hw:0,0",
		},
		{
			name:     "second capture pcm on the same card is listed separately",
			friendly: "HDA Intel PCH — ALC1150 Alt Analog",
			wantID:   "by-path:pci-0000:00:1b.0,2",
			wantKind: DeviceIDByPath, wantStable: true,
			wantChannels: 0, wantALSA: "hw:0,2",
		},
		{
			name:     "card with no symlink is flagged unstable",
			friendly: "Orphan Codec — Orphan Analog",
			wantID:   "card-id:Orphan,0",
			wantKind: DeviceIDCardID, wantStable: false,
			wantChannels: 0, wantALSA: "hw:2,0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, ok := byName[tc.friendly]
			if !ok {
				t.Fatalf("no device named %q; got %v", tc.friendly, keys(byName))
			}
			if d.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", d.ID, tc.wantID)
			}
			if d.IDKind != tc.wantKind {
				t.Errorf("IDKind = %q, want %q", d.IDKind, tc.wantKind)
			}
			if d.Stable != tc.wantStable {
				t.Errorf("Stable = %v, want %v", d.Stable, tc.wantStable)
			}
			if d.Channels != tc.wantChannels {
				t.Errorf("Channels = %d, want %d", d.Channels, tc.wantChannels)
			}
			if d.ALSAName != tc.wantALSA {
				t.Errorf("ALSAName = %q, want %q", d.ALSAName, tc.wantALSA)
			}
		})
	}

	// §6.1: a card index must never BE the identity, only diagnostic detail, and
	// two capture PCMs on one card must not collapse to a single identifier.
	seen := map[string]bool{}
	for _, d := range devs {
		if d.ALSAName == d.ID {
			t.Errorf("device %q uses its card index as identity", d.ID)
		}
		if seen[d.ID] {
			t.Errorf("duplicate device id %q — inputs would be indistinguishable", d.ID)
		}
		seen[d.ID] = true
	}
}

// TestEnumeratePrefersExactPCMLink checks a by-id link pointing at the capture
// PCM node itself beats one pointing only at the card's control node — a USB
// device with several capture PCMs must not collapse to one identifier.
func TestEnumeratePrefersExactPCMLink(t *testing.T) {
	root := t.TempDir()
	proc, dev := filepath.Join(root, "proc"), filepath.Join(root, "dev")
	writeFile(t, filepath.Join(proc, "cards"), " 0 [Multi          ]: USB-Audio - Multi Input\n")
	writeFile(t, filepath.Join(proc, "card0", "id"), "Multi\n")
	writeFile(t, filepath.Join(proc, "card0", "pcm0c", "info"), pcmInfoFile(0, 0, "CAPTURE", "Multi In A"))
	writeFile(t, filepath.Join(proc, "card0", "pcm1c", "info"), pcmInfoFile(0, 1, "CAPTURE", "Multi In B"))
	symlink(t, filepath.Join(dev, "by-id"), "usb-Multi-00", "../controlC0")
	symlink(t, filepath.Join(dev, "by-id"), "usb-Multi-01", "../pcmC0D1c")

	devs, err := (&ALSAEnumerator{ProcRoot: proc, DevRoot: dev}).Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	got := map[string]string{}
	for _, d := range devs {
		got[d.ALSAName] = d.ID
	}
	if want := "by-id:usb-Multi-00,0"; got["hw:0,0"] != want {
		t.Errorf("hw:0,0 ID = %q, want %q", got["hw:0,0"], want)
	}
	if want := "by-id:usb-Multi-01"; got["hw:0,1"] != want {
		t.Errorf("hw:0,1 ID = %q, want %q (the exact PCM link must win)", got["hw:0,1"], want)
	}
}

// TestEnumerateMissingALSAIsAnError asserts a host with no ALSA at all reports an
// error rather than an empty list — "no sound subsystem" must never be rendered
// as "no inputs attached" (guardrail #7).
func TestEnumerateMissingALSAIsAnError(t *testing.T) {
	e := &ALSAEnumerator{ProcRoot: filepath.Join(t.TempDir(), "absent"), DevRoot: t.TempDir()}
	if _, err := e.Devices(); err == nil {
		t.Fatal("expected an error when /proc/asound is absent")
	}
}

// TestEnumerateHost runs the enumerator against this machine's real ALSA state
// when it has one. It asserts only invariants that must hold on any host — it
// does not assume a particular card is present.
func TestEnumerateHost(t *testing.T) {
	if _, err := os.Stat("/proc/asound"); err != nil {
		t.Skip("no /proc/asound on this host")
	}
	devs, err := Devices()
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	seen := map[string]bool{}
	for _, d := range devs {
		if d.ID == "" || d.IDKind == "" {
			t.Errorf("device %+v has no identifier", d)
		}
		if seen[d.ID] {
			t.Errorf("duplicate device id %q on this host", d.ID)
		}
		seen[d.ID] = true
		if !strings.HasPrefix(d.ALSAName, "hw:") {
			t.Errorf("device %q ALSAName = %q, want an hw:card,device string", d.ID, d.ALSAName)
		}
		if d.Channels < 0 {
			t.Errorf("device %q reports negative channels", d.ID)
		}
	}
	t.Logf("enumerated %d capture device(s) on this host", len(devs))
}

func keys(m map[string]Device) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
