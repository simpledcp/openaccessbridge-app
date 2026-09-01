package capture

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// DeviceIDKind records HOW a device's identifier was derived (§6.1: "stable
// device identifiers (by-id / by-path), not card indices that renumber on
// reboot"). The kind travels with the ID so the admin UI can say plainly when
// the only identifier available is a weak one.
type DeviceIDKind string

const (
	// DeviceIDByID is a /dev/snd/by-id link — the strongest identifier: it is
	// derived from the device's own vendor/product/serial strings and survives
	// both reboots and a move to another USB port.
	DeviceIDByID DeviceIDKind = "by-id"
	// DeviceIDByPath is a /dev/snd/by-path link — stable across reboots but
	// tied to the physical port, so re-cabling to another port changes it.
	DeviceIDByPath DeviceIDKind = "by-path"
	// DeviceIDCardID is the kernel's ALSA card id (e.g. "PCH"). Last resort for
	// on-board devices that have neither symlink; it is NOT guaranteed unique
	// or reboot-stable when two identical cards are present, so devices with
	// this kind report Stable=false and the wizard must warn (§6.4 step 1).
	DeviceIDCardID DeviceIDKind = "card-id"
)

// Device is one detected audio CAPTURE input (§6.1, §6.4 step 1 "Select input
// device (from detected devices, with friendly names)"). It carries no audio.
type Device struct {
	// ID is the stable identifier persisted in the auditorium profile (§6.1).
	// It is never a card index.
	ID     string       `json:"id"`
	IDKind DeviceIDKind `json:"id_kind"`
	// Stable reports whether ID is expected to survive a reboot. False means the
	// device exposed neither a by-id nor a by-path link (see DeviceIDCardID).
	Stable bool `json:"stable"`
	// FriendlyName is the human label for the wizard, e.g.
	// "HDA Intel PCH — ALC1150 Analog".
	FriendlyName string `json:"friendly_name"`
	// CardIndex/DeviceIndex are the CURRENT kernel indices. They are diagnostic
	// only and must never be persisted as identity — they renumber on reboot.
	CardIndex   int `json:"card_index"`
	DeviceIndex int `json:"device_index"`
	// Channels is the detected capture channel count (§6.1 "per-input channel
	// count detected (mono vs stereo)"). It is 0 when the kernel does not expose
	// the count without opening the device (typical for on-board HDA codecs) —
	// 0 means UNKNOWN, never "mono".
	Channels int `json:"channels"`
	// ALSAName is the hw:card,device string used to OPEN the device. It is
	// resolved fresh from the stable ID at open time and never persisted (§6.1).
	ALSAName string `json:"alsa_name"`
}

// Enumerator lists the capture devices attached to the appliance. It is an
// interface so the pipeline and the calibration wizard can be tested against a
// fixture tree instead of this host's sound cards.
type Enumerator interface {
	Devices() ([]Device, error)
}

// ALSAEnumerator enumerates capture devices from the kernel's own ALSA state.
// It reads only /proc/asound (card list, per-PCM info, USB stream descriptors)
// and the /dev/snd symlink directories — it never opens a PCM device, so it is
// safe to call while a show is running and needs no audio-group privileges.
//
// Opening a capture stream on real hardware stays hardware-gated (BLOCKERS
// B3/B4); enumeration, the ID scheme, and the friendly names are pure software
// and are exercised by fixture tests.
type ALSAEnumerator struct {
	// ProcRoot defaults to /proc/asound; DevRoot defaults to /dev/snd. Tests
	// point both at a fixture tree.
	ProcRoot string
	DevRoot  string
}

// Devices enumerates the host's capture devices with the default roots.
func Devices() ([]Device, error) { return (&ALSAEnumerator{}).Devices() }

func (e *ALSAEnumerator) procRoot() string {
	if e.ProcRoot == "" {
		return "/proc/asound"
	}
	return e.ProcRoot
}

func (e *ALSAEnumerator) devRoot() string {
	if e.DevRoot == "" {
		return "/dev/snd"
	}
	return e.DevRoot
}

// Devices returns every capture-capable ALSA device, sorted by ID. It reports an
// error (rather than an empty list) when ALSA is not present at all, so "no
// sound subsystem" is never mistaken for "no inputs attached" (guardrail #7).
func (e *ALSAEnumerator) Devices() ([]Device, error) {
	root := e.procRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("capture: ALSA not available at %s: %w", root, err)
	}
	// A directory that is there but holds none of ALSA's own files is not an
	// ALSA tree. A container runtime that masks /proc/asound mounts an empty,
	// read-only tmpfs over it, and reading that as "no cards" would report a
	// booth with no inputs attached when the sound subsystem was merely hidden
	// — the exact confusion the error above exists to prevent (guardrail #7).
	if _, err := os.Stat(filepath.Join(root, "cards")); err != nil {
		return nil, fmt.Errorf("capture: %s holds no ALSA card list — the sound subsystem is hidden from this process "+
			"(a container runtime masking /proc/asound? see deploy/README.md): %w", root, err)
	}
	longNames := readCardNames(filepath.Join(root, "cards"))
	byID := readLinkDir(filepath.Join(e.devRoot(), "by-id"))
	byPath := readLinkDir(filepath.Join(e.devRoot(), "by-path"))

	var out []Device
	for _, ent := range entries {
		if !ent.IsDir() || !strings.HasPrefix(ent.Name(), "card") {
			continue
		}
		card, err := strconv.Atoi(strings.TrimPrefix(ent.Name(), "card"))
		if err != nil {
			continue
		}
		cardDir := filepath.Join(root, ent.Name())
		cardName := longNames[card]
		if cardName == "" {
			cardName = strings.TrimSpace(readFileString(filepath.Join(cardDir, "id")))
		}
		channels := usbCaptureChannels(cardDir)
		for _, pcm := range capturePCMs(cardDir) {
			d := Device{
				CardIndex:    card,
				DeviceIndex:  pcm.device,
				Channels:     channels,
				FriendlyName: friendlyName(cardName, pcm.name),
				ALSAName:     fmt.Sprintf("hw:%d,%d", card, pcm.device),
			}
			d.ID, d.IDKind, d.Stable = stableID(card, pcm.device, byID, byPath, readFileString(filepath.Join(cardDir, "id")))
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].DeviceIndex < out[j].DeviceIndex
	})
	return out, nil
}

// stableID picks the strongest available identifier for a card/device: a by-id
// link for the exact PCM, else a by-id link for the card, else the same two for
// by-path, else the ALSA card id (flagged unstable). A card INDEX is never used
// as identity (§6.1).
//
// Most cards publish a link only for their control node, which names the CARD
// rather than one of its capture PCMs; the device index is then appended so two
// capture PCMs on one card stay distinguishable. The device index is a
// within-card ordinal fixed by the hardware, not the card index that renumbers
// across reboots, so the identifier as a whole remains stable.
func stableID(card, device int, byID, byPath map[string]string, cardID string) (string, DeviceIDKind, bool) {
	pcmNode := fmt.Sprintf("pcmC%dD%dc", card, device)
	ctlNode := fmt.Sprintf("controlC%d", card)
	for _, c := range []struct {
		links map[string]string
		kind  DeviceIDKind
	}{{byID, DeviceIDByID}, {byPath, DeviceIDByPath}} {
		if link, ok := c.links[pcmNode]; ok {
			return string(c.kind) + ":" + link, c.kind, true
		}
		if link, ok := c.links[ctlNode]; ok {
			return fmt.Sprintf("%s:%s,%d", c.kind, link, device), c.kind, true
		}
	}
	id := strings.TrimSpace(cardID)
	if id == "" {
		id = fmt.Sprintf("card%d", card)
	}
	return fmt.Sprintf("%s:%s,%d", DeviceIDCardID, id, device), DeviceIDCardID, false
}

// pcmInfo is one parsed /proc/asound/cardN/pcmXc/info file.
type pcmInfo struct {
	device int
	name   string
}

// capturePCMs lists the capture PCMs of one card. The kernel names capture PCM
// directories pcm<N>c (playback is pcm<N>p), and each carries an `info` file
// with the device index and a human name.
func capturePCMs(cardDir string) []pcmInfo {
	ents, err := os.ReadDir(cardDir)
	if err != nil {
		return nil
	}
	var out []pcmInfo
	for _, ent := range ents {
		name := ent.Name()
		if !ent.IsDir() || !strings.HasPrefix(name, "pcm") || !strings.HasSuffix(name, "c") {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "pcm"), "c"))
		if err != nil {
			continue
		}
		info := parseKeyValues(readFileString(filepath.Join(cardDir, name, "info")))
		if s, ok := info["stream"]; ok && !strings.EqualFold(s, "CAPTURE") {
			continue
		}
		if d, err := strconv.Atoi(info["device"]); err == nil {
			idx = d
		}
		out = append(out, pcmInfo{device: idx, name: info["name"]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].device < out[j].device })
	return out
}

// usbCaptureChannels reads the channel count out of a USB-audio card's stream
// descriptor (/proc/asound/cardN/stream*), which snd-usb-audio publishes without
// opening the device. It returns the highest capture channel count offered by
// any altsetting, or 0 when the card publishes no stream descriptor (on-board
// HDA codecs do not) — 0 means UNKNOWN, and callers must not read it as mono.
//
// The parse is exercised by a fixture test; confirming it against a real USB
// interface is part of the hardware-gated Phase 0C work (BLOCKERS B3).
func usbCaptureChannels(cardDir string) int {
	ents, err := os.ReadDir(cardDir)
	if err != nil {
		return 0
	}
	best := 0
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasPrefix(ent.Name(), "stream") {
			continue
		}
		inCapture := false
		for _, line := range strings.Split(readFileString(filepath.Join(cardDir, ent.Name())), "\n") {
			trimmed := strings.TrimSpace(line)
			switch {
			case trimmed == "Capture:":
				inCapture = true
				continue
			case trimmed == "Playback:":
				inCapture = false
				continue
			}
			if !inCapture || !strings.HasPrefix(trimmed, "Channels:") {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(trimmed, "Channels:"))); err == nil && n > best {
				best = n
			}
		}
	}
	return best
}

// readCardNames parses /proc/asound/cards into card index → long friendly name.
// The file pairs an index/short-id line with an indented long-description line:
//
//	0 [PCH            ]: HDA-Intel - HDA Intel PCH
//	                     HDA Intel PCH at 0xfb130000 irq 57
func readCardNames(path string) map[int]string {
	names := map[int]string{}
	for _, line := range strings.Split(readFileString(path), "\n") {
		open := strings.Index(line, "[")
		end := strings.Index(line, "]:")
		if open < 0 || end < open {
			continue
		}
		idx, err := strconv.Atoi(strings.TrimSpace(line[:open]))
		if err != nil {
			continue
		}
		rest := strings.TrimSpace(line[end+2:])
		if _, long, ok := strings.Cut(rest, " - "); ok {
			rest = long
		}
		names[idx] = strings.TrimSpace(rest)
	}
	return names
}

// readLinkDir maps the basename of each symlink target in dir to the link name,
// e.g. "controlC1" → "pci-0000:01:00.1". A missing directory is not an error:
// /dev/snd/by-id only appears once a device that publishes serial strings (a USB
// interface) is attached.
func readLinkDir(dir string) map[string]string {
	out := map[string]string{}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	names := make([]string, 0, len(ents))
	for _, ent := range ents {
		names = append(names, ent.Name())
	}
	sort.Strings(names) // deterministic winner when two links target one node
	for _, name := range names {
		target, err := os.Readlink(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		key := filepath.Base(target)
		if _, seen := out[key]; !seen {
			out[key] = name
		}
	}
	return out
}

// friendlyName joins the card and PCM descriptions, avoiding the common case
// where both strings are identical.
func friendlyName(card, pcm string) string {
	card, pcm = strings.TrimSpace(card), strings.TrimSpace(pcm)
	switch {
	case card == "" && pcm == "":
		return "unknown audio input"
	case pcm == "" || card == pcm:
		return firstNonEmpty(card, pcm)
	case card == "":
		return pcm
	}
	return card + " — " + pcm
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseKeyValues parses the kernel's "key: value" proc format.
func parseKeyValues(s string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func readFileString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}
