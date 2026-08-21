// Package config is the persisted configuration store (buildplan.md §10, §10.8).
// It persists CONFIGURATION AND PROFILES ONLY — never caption text, audio, asset
// paths that embed titles, or show identifiers beyond what configuration needs
// (§9.4). The model types it stores (AuditoriumProfile, SystemConfigVersion) have
// no content fields by construction; a guard test asserts the persisted bytes
// carry no content.
package config

import (
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// AdminCredential is a stored admin login. The password is never stored in the
// clear — only its PBKDF2 hash (credentials.go).
type AdminCredential struct {
	Username   string
	PWHash     string
	MustChange bool
}

// Store is the persisted configuration interface. Implementations must be
// crash-safe and must never persist content.
type Store interface {
	// Auditorium profiles (§10.5).
	PutAuditoriumProfile(p model.AuditoriumProfile) error
	GetAuditoriumProfile(id string) (model.AuditoriumProfile, bool, error)
	ListAuditoriumProfiles() ([]model.AuditoriumProfile, error)
	DeleteAuditoriumProfile(id string) error

	// System config / versioning record (§10.8).
	GetSystemConfig() (model.SystemConfigVersion, error)
	PutSystemConfig(v model.SystemConfigVersion) error

	// Admin credentials (§8.1 — no default creds past setup).
	PutAdminCredential(c AdminCredential) error
	GetAdminCredential(username string) (AdminCredential, bool, error)
	HasAnyAdminCredential() (bool, error)
	// HasDefaultCredential reports whether any credential is still flagged
	// must_change (i.e. a default that must be replaced before leaving setup).
	HasDefaultCredential() (bool, error)

	// Free-form settings (interface assignment, active firewall profile, setup
	// state, etc.). Values are configuration strings, never content.
	GetSetting(key string) (string, bool, error)
	PutSetting(key, value string) error

	Close() error
}

// Setting keys used across the appliance.
const (
	SettingTheaterInterface = "theater_interface"
	SettingClientInterface  = "client_interface"
	SettingActiveProfile    = "active_firewall_profile"
	SettingSetupComplete    = "setup_complete"
	SettingAdminBindAddr    = "admin_bind_addr"
	SettingPatronBindAddr   = "patron_bind_addr"
	SettingSupportMode      = "support_mode"
	// §9.4 signed offline updates. The trust store holds PUBLIC keys only, as
	// "<id>:<base64 ed25519 public key>" entries separated by whitespace or
	// commas — this appliance verifies signatures and can never make one.
	SettingUpdateTrustedKeys = "update_trusted_keys"
	// SettingUpdateMediaDir is the mount point removable update media appears at.
	SettingUpdateMediaDir = "update_media_dir"
	// SettingPhase9Review records the §12.4 / §1.3 operational and legal review a
	// Low-Cost Replacement deployment may only ship after.
	//
	// The appliance cannot verify that a review happened — it is an
	// organisational fact, not a measurement — so this holds the operator's own
	// reference (a ticket, a date, a signatory) and the surfaces say plainly that
	// it is a RECORD rather than a check. What it does prevent is the claim going
	// unmade: without it the profile that replaces a professional receiver system
	// could be selected by anyone, silently.
	SettingPhase9Review = "phase9_review_reference"
	// SettingAuditoriumName is the room name §7.2 shows a patron and §8.5 relies
	// on as the last human check against a wrong-room join.
	//
	// It is a SETTING rather than only a flag because the shipped systemd unit
	// passes no --auditorium: every appliance installed through the wizard
	// rendered the flag's build-time default, so every screen in a multiplex told
	// its patrons the same room name — the exact confusion §8.5 exists to catch.
	SettingAuditoriumName = "auditorium_name"
	// §9.5 patron Wi-Fi onboarding. The passphrase IS stored, and that is a
	// deliberate decision (D35): a patron network's passphrase is handed to every
	// person in the room and printed on a lobby sign, so it is not a secret in the
	// security sense — and §9.5 asks plainly for a QR that carries it. It is never
	// logged, never served on the patron listener, and appears only in the Wi-Fi
	// QR and on the authenticated admin page.
	SettingPatronWiFiSSID     = "patron_wifi_ssid"
	SettingPatronWiFiPassword = "patron_wifi_password"
	SettingPatronWiFiSecurity = "patron_wifi_security" // WPA | WEP | nopass
	SettingPatronWiFiHidden   = "patron_wifi_hidden"
	// SettingApplianceTier is the §4.3 HARDWARE tier this appliance is —
	// "standard" or "hardened" — which a §9.4 bundle may target. Empty means an
	// operator has not said, and a bundle that targets a tier is then refused
	// rather than installed on a guess.
	SettingApplianceTier = "appliance_tier"
	// SettingUpdateSlots holds the A/B state as JSON (update.SlotStatus). A
	// settings key rather than a table: it is one small record with one writer.
	SettingUpdateSlots = "update_slots"
	SettingSSHEnabled  = "ssh_enabled"

	// Caption-link tuning (§5.2). These have working defaults in
	// internal/caption/acsp; the settings exist so a venue whose server has a
	// different status cadence can move them without a rebuild, rather than the
	// defaults being hardcoded by omission.
	//
	// SettingLeaseWarnFraction: warn once the remaining lease drops below this
	// fraction of the granted lease ("treat anything approaching the lease as a
	// warning", §5.2 item 1). Stored as a decimal string, e.g. "0.25".
	SettingLeaseWarnFraction = "caption_lease_warn_fraction"
	// SettingAssetFetchTimeoutMS bounds ONE RPL / timed-text fetch made from the
	// request path, in milliseconds. It is further clamped to the remaining lease
	// by the state machine.
	SettingAssetFetchTimeoutMS = "caption_asset_fetch_timeout_ms"
)
