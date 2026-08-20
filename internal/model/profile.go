package model

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
	// ACSInitiated: CaptionBridge (the ACS) opens the TCP connection (Variant A).
	ACSInitiated ACSPInitiation = "acs_initiated"
	// ServerInitiated: the media block opens the connection to CaptionBridge
	// (Variant B).
	ServerInitiated ACSPInitiation = "server_initiated"
)

// AuditoriumProfile is persisted configuration (§10.5). It carries device,
// mapping, server identity, and wrong-room safeguards — but NO caption text,
// audio, or title-bearing show data.
type AuditoriumProfile struct {
	AuditoriumID   string `json:"auditorium_id"`
	InputDeviceID  string `json:"input_device_id"`
	ChannelMapping string `json:"channel_mapping"` // e.g. "stereo:L=HI,R=VI-N"
	GainNotes      string `json:"gain_notes"`
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
