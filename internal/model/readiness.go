package model

import (
	"strings"
	"time"
)

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
	// WarnPatronInterfaceUnusable: the interface a patron's phone must reach the
	// appliance on cannot be gathered on — it is missing, down, or unaddressed.
	// The audio inputs go on qualifying perfectly while no patron can subscribe
	// to any of them, so this cannot be inferred from the stream states (§9.1,
	// D27).
	WarnPatronInterfaceUnusable = "patron_interface_unusable"
	// WarnICEScopeUnconfined: the appliance offered patrons candidates on more
	// than one of its networks. Not a local-only violation (no STUN, no TURN),
	// but every extra candidate is join time a phone spends timing out, and it
	// publishes the venue's private topology to the patron LAN (D27).
	WarnICEScopeUnconfined = "ice_candidates_not_confined_to_patron_network"
	// WarnProcessUnreachable covers §3.5's supervised-process liveness: core
	// supervises the media and firewall helpers and reports each one's liveness,
	// so a crashed helper must not read as "silent inputs" or "firewall down".
	WarnProcessUnreachable = "supervised_process_unreachable"
)

const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// Tri is a three-valued dashboard fact: measured true, measured false, or NOT
// MEASURED. Several §8.2 rows read "yes/no", and a bool cannot tell "we looked
// and the answer is no" apart from "nothing ever looked". The whole reason the
// two prior waves exist is that an unmeasured fact rendered as a verdict is a
// fabrication, so the rows that can be unmeasured carry this instead of a bool.
//
// The zero value is TriUnknown on purpose: a field nobody fills renders as "not
// measured" rather than as a green tick.
type Tri string

const (
	TriUnknown Tri = ""
	TriYes     Tri = "yes"
	TriNo      Tri = "no"
)

// TriOf lifts a MEASURED boolean into a Tri. Only call it where the boolean is
// the result of an observation; a default value must stay TriUnknown.
func TriOf(b bool) Tri {
	if b {
		return TriYes
	}
	return TriNo
}

// Label renders a Tri for an operator. "not measured" is spelled out, never
// abbreviated into something that could be read as a verdict.
func (t Tri) Label() string {
	switch t {
	case TriYes:
		return "yes"
	case TriNo:
		return "no"
	default:
		return "not measured"
	}
}

// ICEScopeReport is what the dashboard is allowed to say about ICE scoping (§8.2).
//
// Configured/InForce describe INTENT and MECHANISM. Offered* are MEASUREMENTS,
// read back out of the last answer this hub actually produced. A booth that sees
// "confined: yes" alongside two offered interfaces is looking at a bug, and the
// point of reporting both is that such a disagreement is visible rather than
// hidden behind whichever one the code trusted.
type ICEScopeReport struct {
	// Configured: an operator chose a patron interface.
	Configured bool `json:"configured"`
	// Interface is that choice, echoed back so the dashboard names it.
	Interface string `json:"interface,omitempty"`
	// InForce: the gathering filter is installed and the interface was usable at
	// the last check.
	InForce bool `json:"in_force"`
	// Unusable is the reason gathering cannot happen on the configured
	// interface, if that is the case. Empty when there is no fault.
	Unusable string `json:"unusable,omitempty"`
	// Answered reports whether an answer has been produced at all. Without it,
	// the two counts below are zero because nothing has been measured — never
	// because a patron was offered nothing.
	Answered bool `json:"answered"`
	// OfferedInterfaces are the interfaces whose addresses appeared in the last
	// answer, sorted. More than one means a patron was told to try more than one
	// of this appliance's networks.
	OfferedInterfaces []string `json:"offered_interfaces,omitempty"`
	// OfferedCandidates counts the host candidates in that answer.
	OfferedCandidates int `json:"offered_candidates"`
}

// AudioStreamSummary is one §8.2 audio row derived from the media process's
// per-input measurement (§10.2). It exists because §8.2 asks for facts —
// "HI stream active", "Connected audio clients", "Estimated audio latency" —
// that are DERIVED from the measurements rather than carried by them, and the
// derivation is where honesty about what was not measured has to live.
type AudioStreamSummary struct {
	StreamType StreamType `json:"stream_type"`
	// InputDeviceID is the identity of the input these figures were measured
	// from, and SyntheticSource says that identity is a STAND-IN the media
	// process generated rather than a real capture device (BLOCKERS B3/B4).
	//
	// They are on the summary because §8.2's audio rows are read as "how the
	// AUDITORIUM's feed measured". captionbridge-media refuses to report a
	// synthetic tone under a real device's id — that refusal is the fix for this
	// codebase's first fabrication — but the refusal is worth nothing if the
	// surface that shows "excellent" drops the identity on the way out. A
	// dashboard that hides the source turns a generated tone back into the
	// booth's feed one layer further out.
	InputDeviceID   string `json:"input_device_id,omitempty"`
	SyntheticSource bool   `json:"synthetic_source,omitempty"`
	// InputPresent is §8.2's "HI/VI input present" — measured by the capture
	// health seam, never assumed.
	InputPresent Tri `json:"input_present"`
	// StreamActive is §8.2's "HI/VI stream active": is this stream actually
	// reaching patrons. An encode path that has not been exercised, or an
	// encoder state this build does not recognise, is NOT evidence of a live
	// stream and reports as not measured.
	StreamActive Tri            `json:"stream_active"`
	Quality      QualifierState `json:"quality,omitempty"`
	// Clients is §8.2's "Connected audio clients" for this stream.
	Clients int `json:"connected_clients"`
	// LatencyMeasured says whether an END-TO-END latency was measured. It is
	// false until the Phase 0C loopback calibration runs on real hardware
	// (BLOCKER B3), and while it is false LatencyMS is meaningless: the
	// dashboard must render "not measured", never the estimate dressed up as a
	// measurement (§6.7 / D10 budgets are glass-to-ear).
	LatencyMeasured bool `json:"latency_measured"`
	LatencyMS       int  `json:"latency_ms,omitempty"`
	// EncodePathEstimateMS is the appliance-side ENCODE-PATH estimate only. It
	// is reported as its own labelled figure so it can never be mistaken for the
	// row above.
	EncodePathEstimateMS int    `json:"encode_path_estimate_ms,omitempty"`
	EncoderState         string `json:"encoder_state,omitempty"`
}

// SyntheticDevicePrefix marks the stand-in input identities captionbridge-media
// generates when it has no capture backend for a real device (BLOCKERS B3/B4).
// That process refuses to report a synthetic tone under a real device's id, so
// the prefix is the ONLY signal that a green "excellent" describes a generated
// tone rather than the auditorium.
//
// SEAM: captionbridge-media declares the same literal privately. The two must
// not drift; the constant lives here so every reader derives the fact once, and
// wiring the media process to this one is a one-line change in a package this
// lane does not own.
const SyntheticDevicePrefix = "synthetic-"

// SyntheticDevice reports whether an input identity is a generated stand-in.
func SyntheticDevice(deviceID string) bool {
	return strings.HasPrefix(deviceID, SyntheticDevicePrefix)
}

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
	// ICEScope is what the WebRTC peer actually offered the last patron who
	// joined (§6.7, D27). It sits beside ClientInterface deliberately: that field
	// is the interface an operator CONFIGURED, this one is the set of networks a
	// phone was really told to try, and the two can disagree.
	ICEScope ICEScopeReport `json:"ice_scope"`
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
	// IdentityCheck is the OUTCOME of the §8.5 wrong-room guard as the ACSP layer
	// reported it (acsp.IdentityState: unknown | unpinned | unverified | match |
	// mismatch). It is carried separately from IdentityStrength because "how
	// strong is the pin" and "did the check run and pass" are different
	// questions, and a guard that never ran must not render as a match.
	IdentityCheck string `json:"server_identity_check,omitempty"`

	// DeploymentProfile / DeploymentTitle name the §12 posture in force, and
	// DeploymentBanner is the §8.4 prominent warning banner the profile requires
	// (empty when it requires none). §8.4 permits admin on the patron network
	// ONLY in Lab and Booth Diagnostic and ONLY behind that banner, so the banner
	// is the entire enforcement mechanism on this surface — a dashboard that
	// never renders it has silently dropped the condition.
	DeploymentProfile FirewallProfile `json:"deployment_profile,omitempty"`
	DeploymentTitle   string          `json:"deployment_profile_title,omitempty"`
	DeploymentBanner  string          `json:"deployment_banner,omitempty"`
	// AdminOnPatronNet is the RAW §8.4 fact: can the admin surface be reached
	// from the patron network. It is deliberately separate from AdminIsolated,
	// which reports whether the binding meets the ACTIVE PROFILE's requirement —
	// and in Lab and Booth Diagnostic that requirement is "none", so AdminIsolated
	// is true there even when admin is bound on the patron interface. Those are
	// the two profiles where the condition actually occurs, so a dashboard that
	// showed only AdminIsolated would say "isolated: yes" about the exact
	// situation §8.4 wrote the banner for.
	AdminOnPatronNet Tri `json:"admin_reachable_from_patron,omitempty"`

	// ServerConnected is §8.2's "Server connected: yes/no". It is derived once,
	// here, from CaptionLinkState.ServerConnected so the dashboard does not
	// re-implement "which ACSP states count as connected" in JavaScript.
	ServerConnected Tri `json:"server_connected,omitempty"`

	// Timeline is §8.2's play/paused/stopped row and CueVisible is its
	// "Current cue: visible / none" row. CueVisible carries VISIBILITY ONLY —
	// never the cue text, on any surface (§9.4 caption.cue.visible_no_text).
	Timeline   TimelineState `json:"timeline_state,omitempty"`
	CueVisible Tri           `json:"current_cue_visible,omitempty"`

	// CaptionDriftBudgetMS is the configured §5.5 / D9 drift warn budget the link
	// state's DriftMSEstimate is judged against. §5.5 asks diagnostics to show the
	// number against its limit rather than as a bare figure. It is NOT turned into
	// a §8.3 alarm: the engine measures drift at each re-anchor, so a legitimate
	// server seek or show loop produces a large "drift" that is not a fault, and
	// a banner that fires on every seek is a banner a booth learns to ignore.
	CaptionDriftBudgetMS int64 `json:"caption_drift_budget_ms,omitempty"`
	// CaptionDriftMeasured says whether CaptionLinkState.DriftMSEstimate is a
	// MEASUREMENT. That field has no "unset": it is an int64 that reads 0 both
	// for perfect sync and for a caption path with no language engine to measure
	// anything, and a dashboard that printed the second as "0 ms" would be
	// reporting perfect sync for a path that is not running (guardrail #7).
	CaptionDriftMeasured Tri `json:"caption_drift_measured,omitempty"`

	// AudioStreams are the derived §8.2 audio rows (present / active / clients /
	// latency), and ConnectedAudioClients is their total.
	AudioStreams          []AudioStreamSummary `json:"audio_streams,omitempty"`
	ConnectedAudioClients int                  `json:"connected_audio_clients"`

	// DiscoveryMode and SupportSession are §8.2's last two rows. Both are Tri:
	// this build has no discovery responder and no way to open a support
	// session, so "off" here means "nothing has enabled it", and anything the
	// appliance cannot observe stays not-measured.
	DiscoveryMode  Tri `json:"discovery_mode,omitempty"`
	SupportSession Tri `json:"support_session,omitempty"`

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

// Wrong-room guard outcomes (§8.5). They mirror acsp.IdentityState, which is the
// package that produces them; they are restated here so the dashboard model can
// render the outcome without the web layer importing the ACSP state machine.
// internal/readiness asserts the two vocabularies stay identical (drift would
// silently turn every outcome into the default branch).
const (
	IdentityCheckUnknown    = "unknown"
	IdentityCheckUnpinned   = "unpinned"
	IdentityCheckUnverified = "unverified"
	IdentityCheckMatch      = "match"
	IdentityCheckMismatch   = "mismatch"
)

// IdentityCheckLabel renders the §8.5 wrong-room outcome. Four of the five
// outcomes are NOT a pass, and each says why: "nobody has announced yet",
// "nothing is pinned", and "pinned but unanswerable" are all distinct setup
// facts, and collapsing them to "no mismatch" is how a guard that never ran
// renders green (§8.5, §10.5).
func (r SystemReadiness) IdentityCheckLabel() string {
	switch r.IdentityCheck {
	case IdentityCheckMatch:
		return "matched the pinned identity"
	case IdentityCheckMismatch:
		return "DOES NOT MATCH the pinned identity — wrong room"
	case IdentityCheckUnverified:
		return "not verified — the server announced nothing to check the pin against"
	case IdentityCheckUnpinned:
		return "not checked — this auditorium pins no server identity"
	case IdentityCheckUnknown, "":
		return "not checked — no server has announced yet"
	default:
		return r.IdentityCheck
	}
}

// IdentityCheckOK reports whether the wrong-room guard actually RAN and PASSED.
// Anything else is not a pass — see IdentityCheckLabel.
func (r SystemReadiness) IdentityCheckOK() bool { return r.IdentityCheck == IdentityCheckMatch }
