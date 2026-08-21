package main

// Core's half of the §9.4 update path: loading the trust store, and reading the
// A/B state.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/update"
)

// updateTrustStore parses the configured update-signing keys.
//
// PUBLIC keys only. There is no configuration key for a private one anywhere in
// this tree, and there must never be: an appliance that could sign an update it
// would then accept is one whose compromise is permanent.
//
// A malformed entry is DROPPED with a log line rather than failing the whole
// store. The alternative — refusing to load any key because one is mistyped —
// would take an appliance from "can verify updates" to "can verify nothing"
// over a typo, and "verifies nothing" fails open in the direction that matters:
// an operator who cannot verify a bundle is tempted to install it unverified.
func updateTrustStore(store *config.SQLiteStore, log *logging.Logger) *update.TrustStore {
	raw, _, _ := store.GetSetting(config.SettingUpdateTrustedKeys)
	var keys []update.TrustedKey
	for _, field := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' ' || r == '\t' || r == '\r'
	}) {
		id, encoded, ok := strings.Cut(strings.TrimSpace(field), ":")
		if !ok || strings.TrimSpace(id) == "" {
			log.Warn(logging.EvProcessUnhealthy,
				logging.Field("phase", "update_key_malformed"),
				logging.Field("reason", "entry is not <id>:<base64 key>"))
			continue
		}
		pub, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
		if err != nil || len(pub) != ed25519.PublicKeySize {
			log.Warn(logging.EvProcessUnhealthy,
				logging.Field("phase", "update_key_malformed"),
				logging.Field("key_id", id),
				logging.Field("reason", "not a base64 ed25519 public key"))
			continue
		}
		keys = append(keys, update.TrustedKey{ID: strings.TrimSpace(id), Public: ed25519.PublicKey(pub)})
	}
	ts, err := update.NewTrustStore(keys...)
	if err != nil {
		log.Error(logging.EvProcessUnhealthy,
			logging.Field("phase", "update_trust_store_rejected"),
			logging.Field("error", err.Error()))
		empty, _ := update.NewTrustStore()
		return empty
	}
	return ts
}

// updateSlotStatus reads the persisted A/B state. An unreadable or absent record
// is reported as the zero value, which has no active slot — and the slot machine
// refuses to stage from one. That is the safe direction: an appliance that has
// forgotten which slot it is running must not choose one.
func updateSlotStatus(store *config.SQLiteStore) update.SlotStatus {
	raw, ok, _ := store.GetSetting(config.SettingUpdateSlots)
	if !ok || strings.TrimSpace(raw) == "" {
		return update.SlotStatus{}
	}
	var st update.SlotStatus
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return update.SlotStatus{}
	}
	return st
}

// newUpdateService builds the service the admin surface uses.
func newUpdateService(store *config.SQLiteStore, log *logging.Logger) *update.Service {
	mediaDir, _, _ := store.GetSetting(config.SettingUpdateMediaDir)
	return update.NewService(update.Options{
		Trust:    updateTrustStore(store, log),
		MediaDir: strings.TrimSpace(mediaDir),
		// No Platform: this image has no A/B slots, and UnsupportedPlatform
		// refuses rather than pretending (BLOCKERS B8).
		Current: func() update.Current {
			// Read on every call so an operator correcting the tier does not need
			// a restart. Build and BuildKnown are carried separately: an appliance
			// that does not know its own build refuses to judge an update rather
			// than treating the absence as "everything is newer" (D32).
			tier, _, _ := store.GetSetting(config.SettingApplianceTier)
			build, known := buildinfo.BuildNumber()
			return update.Current{Build: build, BuildKnown: known, Tier: strings.TrimSpace(tier)}
		},
		Status: func() update.SlotStatus { return updateSlotStatus(store) },
	})
}
