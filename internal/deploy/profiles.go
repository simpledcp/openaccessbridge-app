// Package deploy holds the deployment-profile descriptors (buildplan.md §12) and
// the Loaner Device overlay (§12.5). The four profiles share one codebase; they
// differ in firewall posture, which surfaces are exposed, access control, and
// operational assumptions. These descriptors are configuration — the firewall
// manager (internal/security/firewall) turns the network posture into nftables
// rules; this package captures the operational policy each profile implies.
package deploy

import "github.com/simpledcp/openaccessbridge/internal/model"

// Profile is a deployment posture (§12 / §9.2).
type Profile struct {
	Name        model.FirewallProfile `json:"name"`
	Title       string                `json:"title"`
	Description string                `json:"description"`

	// Network posture.
	TheaterAllowlistOnly  bool `json:"theater_allowlist_only"`
	AdminHiddenFromPatron bool `json:"admin_hidden_from_patron"`
	SSHDisabled           bool `json:"ssh_disabled"`

	// Access control / content handling.
	PerShowToken          bool `json:"per_show_token"`
	PurgeAtShowEnd        bool `json:"purge_at_show_end"`
	NoInternetRebroadcast bool `json:"no_internet_rebroadcast"`

	// Operational requirements.
	RequireCalibration      bool `json:"require_calibration"`
	RequireSignalHealth     bool `json:"require_signal_health"`
	RequireClearErrorStates bool `json:"require_clear_error_states"`

	// Capacity + diagnostics.
	MaxConcurrentClients int  `json:"max_concurrent_clients"`
	WarningBanner        bool `json:"warning_banner"`
	AllowPacketCapture   bool `json:"allow_packet_capture"`
	AllowLocalSSH        bool `json:"allow_local_ssh"`

	// Gate that must clear before this profile may ship to a venue (§12.4 / §1.3).
	RequiresPhase9Review bool `json:"requires_phase9_review"`
}

// Builtin returns the four shipping profiles (§9.2 / §12). Concurrent-client caps
// are the working defaults (DECISIONS D11); they are tuned to real venue sizes.
func Builtin() map[model.FirewallProfile]Profile {
	return map[model.FirewallProfile]Profile{
		model.ProfileLab: {
			Name:                    model.ProfileLab,
			Title:                   "Lab",
			Description:             "Development and bench only. Broader discovery, local SSH, full logs and packet captures permitted. Never for public installs.",
			TheaterAllowlistOnly:    false,
			AdminHiddenFromPatron:   false,
			SSHDisabled:             false,
			PerShowToken:            false,
			PurgeAtShowEnd:          true,
			NoInternetRebroadcast:   true,
			MaxConcurrentClients:    10,
			WarningBanner:           true,
			AllowPacketCapture:      true,
			AllowLocalSSH:           true,
			RequireClearErrorStates: false,
		},
		model.ProfileBoothDiagnostic: {
			Name:                  model.ProfileBoothDiagnostic,
			Title:                 "Booth Diagnostic",
			Description:           "For booth techs/festival staff. Allowlisted server IPs; admin on the configured admin subnet/setup net; no forwarding; SSH disabled by default. The lowest-risk first shippable product (no access-equivalence claim).",
			TheaterAllowlistOnly:  true,
			AdminHiddenFromPatron: false, // admin allowed on configured admin subnet/setup net
			SSHDisabled:           true,
			PerShowToken:          false,
			PurgeAtShowEnd:        true,
			NoInternetRebroadcast: true,
			MaxConcurrentClients:  25,
			// §8.4: admin may be reachable from the patron side ONLY in Lab and
			// Booth Diagnostic, and only behind a prominent warning banner.
			WarningBanner:           true,
			AllowPacketCapture:      false,
			AllowLocalSSH:           false,
			RequireClearErrorStates: true,
		},
		model.ProfilePatronFacing: {
			Name:                    model.ProfilePatronFacing,
			Title:                   "Patron-Facing",
			Description:             "Temporary/festival/pop-up accessibility use. Most locked down: server/IMB IPs only; admin hidden from the public SSID; per-show token; caches + sessions purged at show end.",
			TheaterAllowlistOnly:    true,
			AdminHiddenFromPatron:   true,
			SSHDisabled:             true,
			PerShowToken:            true,
			PurgeAtShowEnd:          true,
			NoInternetRebroadcast:   true,
			MaxConcurrentClients:    50,
			WarningBanner:           false,
			AllowPacketCapture:      false,
			AllowLocalSSH:           false,
			RequireClearErrorStates: true,
		},
		model.ProfileLowCostReplacement: {
			Name:                    model.ProfileLowCostReplacement,
			Title:                   "Low-Cost Accessibility Replacement",
			Description:             "In lieu of a professional receiver system. Adds mandatory audio calibration + signal-health monitoring, per-show token, no internet rebroadcast, clear staff/patron error states. Ships to a venue ONLY after the Phase 9 operational/legal review.",
			TheaterAllowlistOnly:    true,
			AdminHiddenFromPatron:   true,
			SSHDisabled:             true,
			PerShowToken:            true,
			PurgeAtShowEnd:          true,
			NoInternetRebroadcast:   true,
			RequireCalibration:      true,
			RequireSignalHealth:     true,
			RequireClearErrorStates: true,
			MaxConcurrentClients:    100,
			WarningBanner:           false,
			AllowPacketCapture:      false,
			AllowLocalSSH:           false,
			RequiresPhase9Review:    true,
		},
	}
}

// Get returns a builtin profile by name.
func Get(name model.FirewallProfile) (Profile, bool) {
	p, ok := Builtin()[name]
	return p, ok
}

// DefaultProfile is the posture an un-provisioned appliance runs under before
// setup assigns one: Booth Diagnostic, the lowest-risk shipping profile and the
// first-shippable target (DECISIONS D13, §12.2/§16). It is deliberately NOT Lab
// — Lab permits admin on the patron network, local SSH, and packet capture, so
// defaulting to it would give a fresh or misconfigured appliance the weakest
// posture of the four (§9.2).
const DefaultProfile = model.ProfileBoothDiagnostic

// Resolve maps a stored profile name to a known profile, falling back to
// DefaultProfile for an empty or unrecognized value. An unknown name must never
// resolve to a more permissive posture than the operator asked for, so the
// fallback is the safe default rather than the stored string taken on trust.
func Resolve(name model.FirewallProfile) (Profile, bool) {
	if p, ok := Get(name); ok {
		return p, true
	}
	p, _ := Get(DefaultProfile)
	return p, false
}

// LoanerOverlay is the Loaner Device provisioning posture (§12.5): a device-
// provisioning overlay layered on Patron-Facing or Low-Cost Replacement, where the
// venue supplies the endpoints rather than relying on patron BYOD. It dissolves
// several of the hardest BYOD problems and supports a stronger access claim, but
// does NOT remove the Phase 9 review.
type LoanerOverlay struct {
	Enabled                 bool `json:"enabled"`
	PreTrustedCert          bool `json:"pre_trusted_cert"`          // local CA on the fleet → no self-signed warning
	ControlledBrowserFleet  bool `json:"controlled_browser_fleet"`  // known WebRTC/autoplay/Wake-Lock behavior
	SuppliedWiredHeadphones bool `json:"supplied_wired_headphones"` // removes Bluetooth latency + no-headphones failure
	KioskLock               bool `json:"kiosk_lock"`                // MDM/guided-access + preloaded per-room shortcut
}

// ApplicableTo reports whether the Loaner overlay may layer on the given base
// profile (§12.5: Patron-Facing or Low-Cost Replacement only).
func (LoanerOverlay) ApplicableTo(base model.FirewallProfile) bool {
	return base == model.ProfilePatronFacing || base == model.ProfileLowCostReplacement
}

// Apply layers the overlay onto a base profile, returning the adjusted profile.
// The overlay is mostly a provisioning posture; here it strengthens the access
// claim by guaranteeing clear error states and purge-at-show-end.
func (o LoanerOverlay) Apply(base Profile) Profile {
	if !o.Enabled {
		return base
	}
	base.RequireClearErrorStates = true
	base.PurgeAtShowEnd = true
	return base
}
