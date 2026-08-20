package model

import "time"

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

	// Caption-plane codes. Guardrail #7 is not audio-only: a caption-path
	// degradation has to reach the dashboard too. internal/caption/acsp raises
	// the first two through Options.OnWarning and declares the same strings next
	// to the state machine that raises them; this is their canonical home (see
	// acsp.WarnLeaseNearExpiry / acsp.WarnCaptionAssetUnavailable).
	WarnCaptionLeaseNearExpiry  = "caption_lease_near_expiry"
	WarnCaptionAssetUnavailable = "caption_asset_unavailable"
	// WarnCaptionParseDegraded carries the timed-text parser's non-fatal warning
	// classes (§5.4). A defaulted edit rate or a whole reel of cues dropped at the
	// <StartTime> origin changes what a patron sees, so it cannot be log-only.
	WarnCaptionParseDegraded = "caption_asset_parse_degraded"

	// Audio-plane causes §6.6 reports ("device connected/disconnected", "encoder
	// state") and §8.3 must shout about. Without them audio_ready goes false with
	// no named cause — a booth sees "not ready" without "the dongle fell out".
	WarnAudioDeviceDisconnected = "audio_input_device_disconnected"
	WarnAudioEncoderUnavailable = "audio_encoder_unavailable"
	// WarnAudioInputProfileRejected: the media process refused the auditorium's
	// stored input profile (§10.5), so the configured device/mapping is NOT what
	// is running — whatever else the audio inputs happen to report.
	WarnAudioInputProfileRejected = "audio_input_profile_rejected"

	// WarnFirewallUnverified is the "asserted but never measured" case §9.3
	// leaves open. Reusing WarnFirewallInactive would tell an operator the
	// firewall is DOWN when it may well be up; this says only that nothing read
	// the ruleset (D17 fail-loud, never fail-wrong).
	WarnFirewallUnverified = "firewall_active_not_introspected"
	// WarnFirewallApplyRefused: the privileged helper would not install the
	// ruleset this profile derives. Whatever is loaded is therefore NOT what the
	// configuration says, even when the loaded ruleset verifies fine — an older
	// posture that still passes is exactly the silent degradation guardrail #7
	// forbids (§9.1).
	WarnFirewallApplyRefused = "firewall_ruleset_not_applied"
	// WarnProcessUnreachable covers §3.5's supervised-process liveness: core
	// supervises the media and firewall helpers and reports each one's liveness,
	// so a crashed helper must not read as "silent inputs" or "firewall down".
	WarnProcessUnreachable = "supervised_process_unreachable"
)

const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// Process names used for per-process liveness reporting (§3.5).
const (
	ProcessCore     = "captionbridge-core"
	ProcessMedia    = "captionbridge-media"
	ProcessFirewall = "captionbridge-firewall"
)

// ProcessLiveness is one supervised process as core observes it (§3.5: core
// "supervises the others … and reports each one's liveness to the dashboard").
// It is a MEASUREMENT of a loopback round trip, never an inference from whether
// the appliance seems to be working.
type ProcessLiveness struct {
	Name  string `json:"name"`
	Alive bool   `json:"alive"`
	// LastSeen is when the process last answered. Nil means it has never
	// answered since core started — distinct from "answered a while ago and has
	// since gone", which a dashboard should show differently.
	LastSeen *time.Time `json:"last_seen,omitempty"`
	// Detail names the failure class (dial_failed / no_response); it carries no
	// path, no client identifier and no operator free text (§9.4).
	Detail string `json:"detail,omitempty"`
}

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
	// FirewallIntrospected reports whether FirewallActive was MEASURED by reading
	// the loaded ruleset or merely asserted by a caller. A dashboard must not
	// render an unmeasured claim as a verified one (§9.3).
	FirewallIntrospected bool `json:"firewall_introspected"`
	BridgesPresent       bool `json:"bridges_present"`
	AdminIsolated        bool `json:"admin_isolated"`
	DefaultCreds         bool `json:"default_credentials_present"`
	SSHDisabled          bool `json:"ssh_disabled"`

	TheaterInterface string `json:"theater_interface"`
	ClientInterface  string `json:"client_interface"`
	// AllowedServer is the CONFIGURED cinema server for this auditorium — the
	// allowlist entry pinned at setup (§8.2 "Allowed cinema server", §10.5). It is
	// deliberately NOT the connected server's self-reported identity: a wrong-room
	// guard that displays whatever the connected server calls itself can never
	// show a mismatch (§8.5).
	AllowedServer string `json:"allowed_cinema_server"`
	// ConnectedServer is what the server on the other end announced about itself.
	ConnectedServer string `json:"connected_cinema_server"`
	// IdentityStrength labels HOW the pinned identity is established
	// (tls_fingerprint | server_id | composite). §10.5: when only a composite
	// identity is available the UI must label it as such so it is not mistaken for
	// a security property the protocol does not provide.
	IdentityStrength string `json:"identity_strength,omitempty"`

	// Processes is the per-process liveness of the three supervised processes
	// (§3.5). Core reports itself alive by construction — it is the reporter.
	Processes []ProcessLiveness `json:"processes,omitempty"`

	// ConnectedCaptionClients / MaxConcurrentClients are §8.2's client counts and
	// the D11 per-profile cap actually in force in the fan-out. A cap of 0 means
	// unlimited, which no shipping profile configures.
	ConnectedCaptionClients int `json:"connected_caption_clients"`
	MaxConcurrentClients    int `json:"max_concurrent_clients"`

	// Versions is the persisted §10.8 config/versioning record: the schema,
	// firewall and device revisions in play plus the last good and last failed
	// readiness checks, so a tech can see what changed since this last worked.
	Versions SystemConfigVersion `json:"system_config"`

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

// IdentityStrengthLabel renders the pinned-identity strength for an operator,
// spelling out the §10.5 caveat for the composite case rather than leaving the
// UI to infer it. An empty strength means no identity is pinned at all.
func (r SystemReadiness) IdentityStrengthLabel() string {
	switch r.IdentityStrength {
	case IdentityTLSFingerprint:
		return "TLS certificate fingerprint"
	case IdentityServerID:
		return "server-reported identifier"
	case IdentityComposite:
		return "composite identity (IP + family + screen name) — a wrong-room guard, not authentication"
	case "":
		return "not pinned"
	default:
		return r.IdentityStrength
	}
}
