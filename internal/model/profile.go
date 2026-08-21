package model

import (
	"errors"
	"fmt"
	"strings"
)

// FirewallProfile is one of the four shipping firewall/deployment postures
// (§9.2 / §12). These are configuration, sharing one codebase.
type FirewallProfile string

const (
	ProfileLab                FirewallProfile = "lab"
	ProfileBoothDiagnostic    FirewallProfile = "booth_diagnostic"
	ProfilePatronFacing       FirewallProfile = "patron_facing"
	ProfileLowCostReplacement FirewallProfile = "low_cost_replacement"
)

// Production reports whether the profile is a production (locked) profile in
// which admin must not be exposed on the patron network and SSH is disabled
// (§8.4, §9.2). Lab and Booth Diagnostic are non-production.
func (p FirewallProfile) Production() bool {
	// An UNRECOGNIZED profile counts as production. §8.4 makes admin/patron
	// separation a hard boundary — "the patron network is untrusted BYOD;
	// authentication alone is not the boundary" — so a hand-edited, migrated, or
	// corrupted profile name must get the STRICTEST posture, not skip the check.
	// Returning false here made validateBindSeparation pass silently on a typo.
	return !p.Known() || p == ProfilePatronFacing || p == ProfileLowCostReplacement
}

// Known reports whether p is one of the four shipping profiles (§9.2 / §12).
// Every fail-safe decision keys off this rather than off a default case, so an
// unknown posture is answered conservatively instead of permissively.
func (p FirewallProfile) Known() bool {
	switch p {
	case ProfileLab, ProfileBoothDiagnostic, ProfilePatronFacing, ProfileLowCostReplacement:
		return true
	default:
		return false
	}
}

// AdminAllowedOnPatronNet reports whether the admin surface may be bound on the
// patron interface for this profile. Only Lab and Booth Diagnostic allow it, and
// only behind a prominent warning banner (§8.4).
func (p FirewallProfile) AdminAllowedOnPatronNet() bool {
	return p == ProfileLab || p == ProfileBoothDiagnostic
}

// ACSPInitiation records which side opens the ACSP TCP connection. This is a
// Phase 0A finding (§5.1, §9.1); the firewall variant depends on it. Default is
// ACSInitiated (Variant A, the current working assumption) and is configurable.
type ACSPInitiation string

const (
	// ACSInitiated: OpenAccessBridge (the ACS) opens the TCP connection (Variant
	// A).
	ACSInitiated ACSPInitiation = "acs_initiated"
	// ServerInitiated: the media block opens the connection to OpenAccessBridge
	// (Variant B).
	ServerInitiated ACSPInitiation = "server_initiated"
)

// AuditoriumProfile is persisted configuration (§10.5). It carries device,
// mapping, server identity, and wrong-room safeguards — but NO caption text,
// audio, or title-bearing show data.
type AuditoriumProfile struct {
	AuditoriumID string `json:"auditorium_id"`
	// InputDeviceID is the capture device for every single-device mapping — the
	// mono and stereo modes of §6.5, where ChannelMapping says how that one
	// device's channels are split between the named streams.
	InputDeviceID  string `json:"input_device_id"`
	ChannelMapping string `json:"channel_mapping"` // e.g. "stereo:L=HI,R=VI-N"
	// StreamDevices names the capture device for EACH stream, for §6.5's
	// "Multiple USB inputs: Device A = HI, Device B = VI-N".
	//
	// It exists because a room wired that way could not previously be
	// CONFIGURED at all: the mapper implemented multi-device, the calibration
	// wizard offered it, and then the profile had one device field to save it
	// into — so the wizard refused the pairing rather than record half of it.
	// Refusing was the right call while the record could not hold the answer;
	// this is the record holding the answer.
	//
	// Empty for every single-device mapping. When it is populated, InputDeviceID
	// is whichever device the operator calibrated first, and is not authoritative
	// on its own — ValidateInputs is.
	StreamDevices map[StreamType]string `json:"stream_devices,omitempty"`
	GainNotes     string                `json:"gain_notes"`
	// LastQualification is the last saved verdict for the input (§6.4 step 9).
	LastQualification QualifierState `json:"last_qualification"`
	CinemaServerIP    string         `json:"cinema_server_ip"`
	ServerFamily      string         `json:"server_family"`
	ACSPPort          int            `json:"acsp_port"`
	ACSPInitiation    ACSPInitiation `json:"acsp_initiation"`
	// ExpectedScreenName and ExpectedServerFingerprint are wrong-room guards
	// (§8.5, §10.5). The fingerprint may be a weaker composite identity; the UI
	// must label it as such (see model.IdentityStrength).
	ExpectedScreenName        string          `json:"expected_screen_name"`
	ExpectedServerFingerprint string          `json:"expected_server_fingerprint"`
	IdentityStrength          string          `json:"identity_strength"` // tls_fingerprint | server_id | composite
	FirewallProfile           FirewallProfile `json:"firewall_profile"`
	// PhysicalPortLabel ties the profile to a labeled port so a re-cabling that
	// swaps rooms is caught at setup, not mid-show (§8.5).
	PhysicalPortLabel string `json:"physical_port_label"`
	// PreferredLanguages is the venue/show-configured default-language priority
	// list (§5.4 multi-language default priority item 1).
	PreferredLanguages []string `json:"preferred_languages,omitempty"`
}

// Identity strength labels (§10.5 note on expected_server_fingerprint).
const (
	IdentityTLSFingerprint = "tls_fingerprint" // strongest
	IdentityServerID       = "server_id"
	IdentityComposite      = "composite" // IP + family + screen name; not a security property
)

// ValidateInputs checks that the profile's device assignment is coherent for its
// mapping mode (§6.5). It returns nil when the profile is usable.
//
// The three ways a multi-device room can be recorded wrongly, each of which
// would otherwise become a fault during a show rather than before one:
//
//   - a stream with no device, so nothing feeds it and a patron who selects that
//     feed hears nothing;
//   - both streams on the SAME device, which is not multi-device at all — it is a
//     channel mapping, and recording it this way hides which channel each stream
//     is really taking;
//   - devices listed for a mapping that is not multi-device, where they would be
//     silently ignored and the room would run on a mapping nobody chose.
//
// It does NOT judge an empty single-device InputDeviceID: on this build that is
// a real state (no device chosen, so the synthetic stand-in feeds the streams)
// and this type cannot tell it apart from a half-finished configuration.
//
// mode is the parsed §6.5 mapping (mapper.Mode.String()); streams are the named
// streams this auditorium serves.
func (p AuditoriumProfile) ValidateInputs(multiDevice bool, streams []StreamType) error {
	if !multiDevice {
		if len(p.StreamDevices) > 0 {
			return fmt.Errorf("channel mapping %q is a single-device mapping, but the profile lists "+
				"per-stream devices (%d); they would be ignored", p.ChannelMapping, len(p.StreamDevices))
		}
		// An EMPTY InputDeviceID is deliberately not an error here. On this build
		// it is a real and useful state — no device chosen, so the synthetic
		// stand-in feeds the streams — and this type cannot tell that apart from a
		// half-finished configuration. Whichever caller knows the difference
		// decides; cmd/openaccessbridge-media does.
		return nil
	}
	if len(streams) == 0 {
		return errors.New("multi-device mapping: no named streams to assign devices to")
	}
	seen := map[string]StreamType{}
	for _, st := range streams {
		dev := strings.TrimSpace(p.StreamDevices[st])
		if dev == "" {
			return fmt.Errorf("multi-device mapping: no capture device is assigned to %s, so nothing would feed it", st)
		}
		if other, dup := seen[dev]; dup {
			return fmt.Errorf("multi-device mapping: %s and %s are both assigned device %q; "+
				"two streams on one device is a channel mapping, not a multi-device pairing (§6.5)",
				other, st, dev)
		}
		seen[dev] = st
	}
	return nil
}

// InputDeviceFor returns the capture device that feeds one named stream, and
// whether the profile assigns one at all.
func (p AuditoriumProfile) InputDeviceFor(stream StreamType) (string, bool) {
	if dev, ok := p.StreamDevices[stream]; ok && strings.TrimSpace(dev) != "" {
		return strings.TrimSpace(dev), true
	}
	if strings.TrimSpace(p.InputDeviceID) != "" {
		return strings.TrimSpace(p.InputDeviceID), true
	}
	return "", false
}
