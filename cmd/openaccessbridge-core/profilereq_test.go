package main

import (
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// §12.4 makes the calibration wizard mandatory for the replacement profile, and
// the evidence has to be a MEASUREMENT rather than the existence of a profile
// row. An auditorium can be configured — device, mapping, server, everything —
// without anyone ever having measured the input, and that appliance is exactly
// the one this requirement is about.
//
// §10.5's last_qualification is good evidence precisely because the calibration
// lane REFUSES to write it for anything else: a run against the synthetic
// stand-in, or one that measured nothing, leaves it empty. That refusal is what
// makes this checkable at all.
func TestCalibrationEvidenceIsAMeasurementNotAConfiguredProfile(t *testing.T) {
	dir := t.TempDir()

	// A fully configured auditorium that nobody has calibrated.
	store := testStore(t, model.AuditoriumProfile{
		AuditoriumID:   "screen-3",
		CinemaServerIP: "10.10.20.15",
		InputDeviceID:  "synthetic-hi",
		ChannelMapping: "mono:HI",
	})
	sup := newSupervisor(t, store, dir)
	if sup.calibrationRecorded() {
		t.Fatal("a configured but never-measured auditorium counts as calibrated")
	}

	// Now with a verdict a real run wrote.
	if err := store.PutAuditoriumProfile(model.AuditoriumProfile{
		AuditoriumID:      "screen-3",
		CinemaServerIP:    "10.10.20.15",
		InputDeviceID:     "synthetic-hi",
		ChannelMapping:    "mono:HI",
		LastQualification: model.QualExcellent,
	}); err != nil {
		t.Fatal(err)
	}
	if !sup.calibrationRecorded() {
		t.Fatal("a recorded qualification does not count as calibrated")
	}
}

// The Phase 9 record is a record: present or absent, never inferred.
func TestPhase9IsRecordedOrAbsentNeverInferred(t *testing.T) {
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3"})
	sup := newSupervisor(t, store, dir)

	if sup.phase9Recorded() {
		t.Fatal("a fresh appliance reports a Phase 9 review")
	}
	mustSetting(t, store, config.SettingPhase9Review, "   ")
	if sup.phase9Recorded() {
		t.Fatal("whitespace counted as a recorded review")
	}
	mustSetting(t, store, config.SettingPhase9Review, "OPS-441, reviewed 2026-08-01")
	if !sup.phase9Recorded() {
		t.Fatal("a recorded reference does not count")
	}
}
