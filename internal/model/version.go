package model

import "time"

// ReadinessCheckResult records the outcome of a readiness check (§10.8). It
// records a reason string only — never caption text or audio.
type ReadinessCheckResult struct {
	At     time.Time `json:"at"`
	OK     bool      `json:"ok"`
	Reason string    `json:"reason,omitempty"`
}

// SystemConfigVersion is the persisted versioned config/state record (§10.8). It
// answers "what changed since this last worked?" — configuration only, no content.
type SystemConfigVersion struct {
	ConfigSchemaVersion    int                   `json:"config_schema_version"`
	HardwareProfileVersion string                `json:"hardware_profile_version"`
	FirewallProfileVersion string                `json:"firewall_profile_version"`
	ServerFamilyCompat     string                `json:"server_family_compat_profile"`
	AudioDeviceCompat      string                `json:"audio_device_compat_profile"`
	LastReadinessCheckOK   *time.Time            `json:"last_readiness_check_ok,omitempty"`
	LastReadinessCheckFail *ReadinessCheckResult `json:"last_readiness_check_failure,omitempty"`
	UpdateChannel          string                `json:"update_channel"` // stable | beta
}

// CurrentConfigSchemaVersion is the schema version this build writes.
const CurrentConfigSchemaVersion = 1
