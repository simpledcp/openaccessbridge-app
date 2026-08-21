package update

import (
	"errors"
	"testing"
	"time"
)

var at = time.Date(2026, 8, 21, 22, 0, 0, 0, time.UTC)

func staged(t *testing.T) SlotStatus {
	t.Helper()
	s, err := Stage(SlotStatus{Active: SlotA, ActiveBuild: 1000, ActiveVersion: "1.3.0"},
		Manifest{Build: 1420, Version: "1.4.2"}, at)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// An update goes into the OTHER slot. Writing over the running one is not an A/B
// update, it is an update with no way back.
func TestStagingUsesTheInactiveSlot(t *testing.T) {
	s := staged(t)
	if s.Staged != SlotB {
		t.Fatalf("staged into %q while running %q", s.Staged, s.Active)
	}
	if s.Active != SlotA {
		t.Fatal("staging changed the running slot")
	}
	if s.StagedBuild != 1420 {
		t.Fatalf("staged build %d", s.StagedBuild)
	}
}

// Two unproven images in a two-slot system means no known-good one is left,
// which is the entire property A/B exists to provide.
func TestStagingOverAnUnprovenImageIsRefused(t *testing.T) {
	s := staged(t)
	if _, err := Stage(s, Manifest{Build: 1500, Version: "1.5.0"}, at); !errors.Is(err, ErrAlreadyStaged) {
		t.Fatalf("a second image was staged over an unproven one: %v", err)
	}
}

// The good path: the new image boots, confirms itself, and becomes active. The
// slot it replaced becomes the spare.
func TestAConfirmedImageBecomesActive(t *testing.T) {
	s := staged(t)
	s, err := BeginBoot(s)
	if err != nil {
		t.Fatal(err)
	}
	s, err = ConfirmHealthy(s, SlotB, Manifest{Build: 1420, Version: "1.4.2"}, at)
	if err != nil {
		t.Fatal(err)
	}
	if s.Active != SlotB {
		t.Fatalf("the confirmed image did not become active: %+v", s)
	}
	if s.HasStaged() {
		t.Fatal("something is still staged after a commit")
	}
	if s.ActiveBuild != 1420 || s.LastOutcome != OutcomeCommitted {
		t.Fatalf("the commit was not recorded: %+v", s)
	}
	if s.BootAttempts != 0 {
		t.Fatalf("boot attempts were not reset: %d", s.BootAttempts)
	}
}

// THE rule that makes automatic rollback real: the image confirms ITSELF. An
// updater that marks its own work healthy has a rollback that can only fire when
// nothing ran at all — and the failure worth catching is the update that boots,
// comes up, and cannot do its job.
func TestTheOldImageCannotConfirmTheNewOnesHealth(t *testing.T) {
	s := staged(t)
	s, _ = BeginBoot(s)
	// SlotA is the OLD image. It must not be able to bless SlotB.
	if _, err := ConfirmHealthy(s, SlotA, Manifest{Build: 1420}, at); !errors.Is(err, ErrNotStagedSlot) {
		t.Fatalf("the old image confirmed the new one's health: %v", err)
	}
}

// A staged image that never confirms itself is abandoned after its attempts run
// out, and the appliance keeps running the image that was working.
func TestAnImageThatNeverConfirmsIsRolledBack(t *testing.T) {
	s := staged(t)
	for i := 0; i < DefaultMaxBootAttempts; i++ {
		if s.ExhaustedAttempts() {
			t.Fatalf("gave up after %d attempts, the limit is %d", i, DefaultMaxBootAttempts)
		}
		var err error
		if s, err = BeginBoot(s); err != nil {
			t.Fatal(err)
		}
	}
	if !s.ExhaustedAttempts() {
		t.Fatalf("after %d attempts the image is still being retried: %+v", DefaultMaxBootAttempts, s)
	}

	rolled, err := RollBack(s, at)
	if err != nil {
		t.Fatal(err)
	}
	if rolled.Active != SlotA {
		t.Fatalf("a rollback changed the running slot to %q", rolled.Active)
	}
	if rolled.ActiveBuild != 1000 || rolled.ActiveVersion != "1.3.0" {
		t.Fatalf("the known-good version was lost: %+v", rolled)
	}
	if rolled.HasStaged() {
		t.Fatal("the abandoned image is still staged")
	}
	if rolled.LastOutcome != OutcomeRolledBack {
		t.Fatalf("the rollback was not recorded as one: %q", rolled.LastOutcome)
	}
}

// One failed boot must NOT roll back a good update: a power cut mid-start, or a
// tech switching the rack off, would otherwise undo a working install.
func TestASingleFailedBootDoesNotRollBack(t *testing.T) {
	s := staged(t)
	s, _ = BeginBoot(s)
	if s.ExhaustedAttempts() {
		t.Fatal("one interrupted boot abandoned the update")
	}
	// And a confirmation on the next try still commits.
	s, _ = BeginBoot(s)
	if _, err := ConfirmHealthy(s, SlotB, Manifest{Build: 1420}, at); err != nil {
		t.Fatalf("an image that confirmed on its second boot was not committed: %v", err)
	}
}

// The attempt is counted BEFORE control passes to the new image, so one that
// hangs before it can report anything still burns a try. Otherwise the worst
// possible update — one that wedges instantly — retries forever.
func TestAnImageThatHangsStillConsumesItsAttempts(t *testing.T) {
	s := staged(t)
	for i := 0; i < DefaultMaxBootAttempts; i++ {
		s, _ = BeginBoot(s) // nothing ever confirms
	}
	if !s.ExhaustedAttempts() {
		t.Fatal("an image that never reported anything was retried indefinitely")
	}
}

// Nothing staged means nothing to confirm, roll back, or boot into.
func TestTheMachineRefusesTransitionsWithNothingStaged(t *testing.T) {
	s := SlotStatus{Active: SlotA}
	if _, err := BeginBoot(s); !errors.Is(err, ErrNothingStaged) {
		t.Errorf("BeginBoot: %v", err)
	}
	if _, err := ConfirmHealthy(s, SlotA, Manifest{}, at); !errors.Is(err, ErrNothingStaged) {
		t.Errorf("ConfirmHealthy: %v", err)
	}
	if _, err := RollBack(s, at); !errors.Is(err, ErrNothingStaged) {
		t.Errorf("RollBack: %v", err)
	}
	if s.ExhaustedAttempts() {
		t.Error("an appliance with nothing staged reports exhausted attempts")
	}
}

// A platform with no A/B slots must REFUSE, not silently do nothing. An
// appliance that reports a rollback capability it does not have is one a booth
// trusts not to brick — which is exactly the promise §9.4 makes.
func TestAPlatformWithoutSlotsRefusesRatherThanPretending(t *testing.T) {
	p := UnsupportedPlatform{}
	if _, ok := p.Slots(); ok {
		t.Fatal("a platform with no slots claims to have them")
	}
	if err := p.WriteImage(SlotB, []byte("image")); !errors.Is(err, ErrNoRollbackHere) {
		t.Fatalf("WriteImage returned %v", err)
	}
	if err := p.SetBootTarget(SlotB); !errors.Is(err, ErrNoRollbackHere) {
		t.Fatalf("SetBootTarget returned %v", err)
	}
	// The refusal names what is missing, because an operator's next question is
	// what would have to change.
	if err := p.WriteImage(SlotA, nil); err == nil || !contains(err.Error(), "A/B partitions") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}

// An unconfigured active slot is not a slot. Staging from one would put an image
// somewhere nobody chose.
func TestStagingFromAnUnknownActiveSlotIsRefused(t *testing.T) {
	if _, err := Stage(SlotStatus{}, Manifest{Build: 1}, at); !errors.Is(err, ErrInvalidSlot) {
		t.Fatalf("staged from an unset active slot: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
