// Package config is the persisted configuration store (buildplan.md §10, §10.8).
// It persists CONFIGURATION AND PROFILES ONLY — never caption text, audio, asset
// paths that embed titles, or show identifiers beyond what configuration needs
// (§9.4). The model types it stores (AuditoriumProfile, SystemConfigVersion) have
// no content fields by construction; a guard test asserts the persisted bytes
// carry no content.
package config

import (
	"github.com/simpledcp/captionbridge/internal/model"
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
	SettingSSHEnabled       = "ssh_enabled"

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
