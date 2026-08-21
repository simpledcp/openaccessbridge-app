package update

// The A/B slot model with automatic rollback (§9.4: "the hardened tier uses A/B
// partitions for atomic updates with automatic rollback on failed boot/health-
// check, so a bad update never bricks a booth appliance").
//
// WHAT THIS FILE IS. A state machine, complete and tested, over four facts: which
// slot is running, which slot has something staged, how many times the staged
// slot has been tried, and whether it ever confirmed itself healthy. Every rule
// §9.4 asks for is expressible in those terms, and every one of them is a rule
// about what happens when something goes WRONG — which is the half that cannot
// be checked by installing a good update and watching it work.
//
// WHAT THIS FILE IS NOT. It cannot make a machine boot from the other slot. That
// needs a bootloader that reads the same state (grub-editenv, U-Boot, systemd-
// boot's counting), real partitions, and on the hardened tier a TPM to hold the
// key. None of that exists on this image, and none of it is simulated here: the
// Platform seam below refuses on a host that cannot do it, so an appliance can
// never report a rollback capability it does not have. See BLOCKERS B8.
//
// THE RULE THAT MAKES ROLLBACK REAL: health is CONFIRMED by the new image, never
// assumed by the thing that installed it. An updater that marks its own work
// healthy has an automatic rollback that can only trigger on a crash so complete
// that nothing ran — which is the one failure a booth would notice anyway. The
// failure worth catching is the update that boots, comes up, and cannot talk to
// the cinema server.

import (
	"errors"
	"fmt"
	"time"
)

// Slot is one of the two images.
type Slot string

const (
	SlotA Slot = "a"
	SlotB Slot = "b"
)

// Other returns the slot this one is not.
func (s Slot) Other() Slot {
	if s == SlotA {
		return SlotB
	}
	return SlotA
}

// Valid reports whether s names a slot.
func (s Slot) Valid() bool { return s == SlotA || s == SlotB }

// DefaultMaxBootAttempts is how many times a staged image may be tried before
// the appliance gives up on it.
//
// Three, not one: a single failed boot can be a power cut mid-start or a booth
// tech pulling the plug, and rolling back a good update because someone switched
// the rack off is its own kind of outage. Three consecutive attempts without a
// health confirmation is a broken image.
const DefaultMaxBootAttempts = 3

// SlotStatus is the persisted A/B state. It is the whole state machine's memory,
// and it is written before each transition rather than after: a crash between
// deciding and recording must leave the appliance believing the MORE cautious of
// the two states.
type SlotStatus struct {
	// Active is the slot currently running.
	Active Slot `json:"active"`
	// Staged is a slot holding a verified but unproven image, empty when there is
	// none.
	Staged Slot `json:"staged,omitempty"`
	// StagedBuild / StagedVersion describe what is staged, for the operator.
	StagedBuild   int64  `json:"staged_build,omitempty"`
	StagedVersion string `json:"staged_version,omitempty"`
	// ActiveBuild / ActiveVersion describe what is running.
	ActiveBuild   int64  `json:"active_build,omitempty"`
	ActiveVersion string `json:"active_version,omitempty"`
	// BootAttempts counts boots of the staged slot that have not yet confirmed
	// health. It is incremented BEFORE the boot is attempted, so an image that
	// hangs before it can count anything still burns an attempt — otherwise the
	// worst kind of broken update would retry forever.
	BootAttempts int `json:"boot_attempts,omitempty"`
	// MaxBootAttempts is the limit in force; zero means DefaultMaxBootAttempts.
	MaxBootAttempts int `json:"max_boot_attempts,omitempty"`
	// LastOutcome records how the previous attempt ended, so an operator sees
	// "rolled back" rather than an appliance that mysteriously runs the old
	// version.
	LastOutcome string `json:"last_outcome,omitempty"`
	// LastOutcomeAt timestamps it.
	LastOutcomeAt *time.Time `json:"last_outcome_at,omitempty"`
}

// Outcomes recorded in LastOutcome.
const (
	OutcomeCommitted  = "committed"
	OutcomeRolledBack = "rolled_back"
	OutcomeStaged     = "staged"
)

// Errors from the slot machine.
var (
	ErrNothingStaged  = errors.New("update: nothing is staged")
	ErrAlreadyStaged  = errors.New("update: an image is already staged; commit or discard it first")
	ErrNotStagedSlot  = errors.New("update: the running slot is not the staged one")
	ErrInvalidSlot    = errors.New("update: not a valid slot")
	ErrNoRollbackHere = errors.New("update: this appliance has no A/B slots, so nothing can be staged or rolled back")
)

// limit is the boot-attempt ceiling in force.
func (s SlotStatus) limit() int {
	if s.MaxBootAttempts > 0 {
		return s.MaxBootAttempts
	}
	return DefaultMaxBootAttempts
}

// HasStaged reports whether an unproven image is waiting.
func (s SlotStatus) HasStaged() bool { return s.Staged.Valid() }

// ExhaustedAttempts reports whether the staged image has used up its tries. A
// caller that sees this must roll back rather than try again.
func (s SlotStatus) ExhaustedAttempts() bool {
	return s.HasStaged() && s.BootAttempts >= s.limit()
}

// Stage records a verified image into the inactive slot.
//
// It refuses to stage over an existing staged image. Two unproven images in a
// two-slot system means the appliance has no known-good one to fall back to,
// which is the whole property A/B exists to provide.
func Stage(s SlotStatus, m Manifest, now time.Time) (SlotStatus, error) {
	if !s.Active.Valid() {
		return s, fmt.Errorf("%w: active slot is %q", ErrInvalidSlot, s.Active)
	}
	if s.HasStaged() {
		return s, ErrAlreadyStaged
	}
	out := s
	out.Staged = s.Active.Other()
	out.StagedBuild, out.StagedVersion = m.Build, m.Version
	out.BootAttempts = 0
	out.LastOutcome = OutcomeStaged
	t := now.UTC()
	out.LastOutcomeAt = &t
	return out, nil
}

// BeginBoot records that the staged slot is about to be tried. It is called
// BEFORE control passes to the new image, so an image that hangs before it can
// report anything still consumes an attempt.
func BeginBoot(s SlotStatus) (SlotStatus, error) {
	if !s.HasStaged() {
		return s, ErrNothingStaged
	}
	out := s
	out.BootAttempts++
	return out, nil
}

// ConfirmHealthy commits the staged image: it becomes active and the previous
// active slot becomes the spare.
//
// running is the slot that is actually executing. It is a parameter rather than
// something inferred, because the one call that must never be possible is the
// OLD image confirming the health of the NEW one — an update that rolled back and
// then had its predecessor mark it good would leave a broken image committed.
func ConfirmHealthy(s SlotStatus, running Slot, m Manifest, now time.Time) (SlotStatus, error) {
	if !s.HasStaged() {
		return s, ErrNothingStaged
	}
	if running != s.Staged {
		return s, fmt.Errorf("%w: running %q, staged %q", ErrNotStagedSlot, running, s.Staged)
	}
	out := s
	out.Active = s.Staged
	out.ActiveBuild, out.ActiveVersion = m.Build, m.Version
	out.Staged = ""
	out.StagedBuild, out.StagedVersion = 0, ""
	out.BootAttempts = 0
	out.LastOutcome = OutcomeCommitted
	t := now.UTC()
	out.LastOutcomeAt = &t
	return out, nil
}

// RollBack abandons the staged image and keeps the active one. The active slot
// does not change: it never stopped being the known-good image, which is why
// this is a rollback and not a second update.
func RollBack(s SlotStatus, now time.Time) (SlotStatus, error) {
	if !s.HasStaged() {
		return s, ErrNothingStaged
	}
	out := s
	out.Staged = ""
	out.StagedBuild, out.StagedVersion = 0, ""
	out.BootAttempts = 0
	out.LastOutcome = OutcomeRolledBack
	t := now.UTC()
	out.LastOutcomeAt = &t
	return out, nil
}

// Platform is the seam between this state machine and a machine that can
// actually switch what it boots.
//
// Every method may refuse. On this image every one of them does, and that is the
// honest answer rather than a limitation to work around: writing an image into a
// second partition that does not exist, or telling a bootloader that is not
// integrated to try it, would leave an appliance reporting a rollback capability
// it has never had — and a booth trusting that a bad update cannot brick it.
type Platform interface {
	// Slots reports whether this machine has A/B slots at all, and which one is
	// running.
	Slots() (running Slot, ok bool)
	// WriteImage places a verified payload into the inactive slot.
	WriteImage(slot Slot, payload []byte) error
	// SetBootTarget tells the bootloader which slot to try next.
	SetBootTarget(slot Slot) error
}

// UnsupportedPlatform is the Platform for a machine with no A/B slots. It is the
// one this build ships with.
type UnsupportedPlatform struct {
	// Reason is what a booth is told. It names the missing thing rather than
	// saying "unsupported", because the operator's next question is what would
	// have to change.
	Reason string
}

func (u UnsupportedPlatform) Slots() (Slot, bool) { return "", false }

func (u UnsupportedPlatform) WriteImage(Slot, []byte) error { return u.err() }

func (u UnsupportedPlatform) SetBootTarget(Slot) error { return u.err() }

func (u UnsupportedPlatform) err() error {
	reason := u.Reason
	if reason == "" {
		reason = "this image has no A/B partitions and no bootloader integration (BLOCKERS B8)"
	}
	return fmt.Errorf("%w: %s", ErrNoRollbackHere, reason)
}
