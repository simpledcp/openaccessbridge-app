package model

// Warning is a single loud-warning condition for the dashboard (§8.3). The
// dashboard must shout, not whisper.
type Warning struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Severity string `json:"severity"` // critical | warning
}

// Warning codes (§8.3). Each maps to a condition that flips a loud alert.
const (
	WarnForwardingEnabled      = "forwarding_enabled"
	WarnInterfacesBridged      = "interfaces_bridged"
	WarnFirewallInactive       = "firewall_inactive"
	WarnAdminOnPatronNet       = "admin_reachable_on_patron_net"
	WarnDefaultPassword        = "default_password_set"
	WarnSSHEnabled             = "ssh_enabled_outside_support"
	WarnSharedInterface        = "patron_theater_shared_interface"
	WarnDNSLeak                = "dns_internet_leak_across_interfaces"
	WarnAudioSilentInPlayback  = "audio_input_silent_during_playback"
	WarnAudioClipping          = "audio_clipping"
	WarnImpossibleMapping      = "stereo_mapping_on_mono_input"
	WarnAudioLatencyOver       = "audio_latency_over_threshold"
	WarnServerIdentityMismatch = "cinema_server_identity_mismatch"
	WarnDebugCaptureActive     = "debug_capture_or_support_mode_active"
)

const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// SystemReadiness is the composite dashboard verdict (§10.6): two independent
// top-line verdicts plus the loud-warning set. Caption and audio readiness are
// reported SEPARATELY and computed independently (§2 asymmetry, guardrail #7).
type SystemReadiness struct {
	CaptionReady bool `json:"caption_ready"`
	AudioReady   bool `json:"audio_ready"`

	CaptionLink CaptionLinkState   `json:"caption_link"`
	AudioInputs []AudioStreamState `json:"audio_inputs"`

	// Network/security posture (§9.3).
	ForwardingDisabled bool `json:"forwarding_disabled"`
	FirewallActive     bool `json:"firewall_active"`
	BridgesPresent     bool `json:"bridges_present"`
	AdminIsolated      bool `json:"admin_isolated"`
	DefaultCreds       bool `json:"default_credentials_present"`
	SSHDisabled        bool `json:"ssh_disabled"`

	TheaterInterface string `json:"theater_interface"`
	ClientInterface  string `json:"client_interface"`
	AllowedServer    string `json:"allowed_cinema_server"`

	Warnings []Warning `json:"warnings"`
}

// HasCritical reports whether any critical warning is present.
func (r SystemReadiness) HasCritical() bool {
	for _, w := range r.Warnings {
		if w.Severity == SeverityCritical {
			return true
		}
	}
	return false
}
