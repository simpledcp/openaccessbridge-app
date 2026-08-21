package model

// Wire payloads for the §3.5 loopback IPC between the three supervised
// processes. They live here, beside the state they carry, so the two ends of a
// request cannot drift into different shapes: the sender
// (openaccessbridge-core) and the receiver (openaccessbridge-media /
// openaccessbridge-firewall) are separate binaries and would otherwise each
// keep their own copy of the struct.
//
// These carry STATE and COMMANDS only. No caption text, no audio samples, no SDP
// bodies and no client identifiers cross this boundary (§9.4).

// PostureFacts is the caller-supplied half of a §9.3 verification pass: the
// facts about the appliance's own configuration that the kernel does not expose,
// which core knows and the privileged helper does not.
//
// It deliberately carries NO "firewall is active" claim. That fact is measured by
// reading the loaded nftables ruleset (verify.Config.Ruleset), never asserted by
// whoever happens to be asking — a helper answering a ping and a --allow-apply
// flag are not evidence that a default-deny ruleset is loaded (§9.3).
type PostureFacts struct {
	Profile FirewallProfile `json:"profile"`
	// TheaterIface and ClientIface are the two never-joined zones (§9.1); with
	// both set, the helper's pass also runs the route/DNS assertions.
	TheaterIface string `json:"theater_iface,omitempty"`
	ClientIface  string `json:"client_iface,omitempty"`
	// TheaterAllowlist is the configured cinema-server allowlist the theater-side
	// reachable scope is measured against.
	TheaterAllowlist         []string `json:"theater_allowlist,omitempty"`
	AdminReachableFromPatron bool     `json:"admin_reachable_from_patron"`
	DefaultCredentials       bool     `json:"default_credentials"`
	SSHEnabledOutsideSupport bool     `json:"ssh_enabled_outside_support"`
}

// MediaCommandResult is the media helper's reply to configure/start/stop. A
// refusal is reported as OK=false with a named Error rather than a silently
// unchanged pipeline (guardrail #7): if the configured input cannot be honored,
// core has to be able to shout about it.
type MediaCommandResult struct {
	OK bool `json:"ok"`
	// Error names the failure class in plain language. It carries no audio, no
	// device path beyond the configured stable ID, and no operator free text.
	Error string `json:"error,omitempty"`
	// Streams is the per-stream state after the command, so a caller does not
	// have to race a follow-up media.status to learn what happened.
	Streams []AudioStreamState `json:"streams,omitempty"`
}
