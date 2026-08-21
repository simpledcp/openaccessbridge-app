package model

// The §11.5 pre-show QA checklist: the shape of a staff-facing gate, and the
// three-valued honesty that makes it worth running.
//
// §11.5 asks for two things, and the second is the one that is usually skipped:
// "Codify a staff pre-show checklist with explicit pass/fail … Crucially, define
// what staff do when a check fails before a show — readiness checks are only
// useful if they're tied to a procedure." So every item here carries a Remedy.
// An item with no remedy is a fact about the appliance, not a checklist item,
// and it does not belong on a page a person consults ninety seconds before a
// house opens.
//
// THE FAILURE THIS FILE IS SHAPED AGAINST. A checklist is the single most
// inviting place in this product to write down something that was never
// verified: it is a list of green ticks, read under time pressure, by someone
// who wants to open the house. This repo has already shipped that bug once — a
// §10.8 readiness stamp written by pressing "Complete" rather than by a check
// that ran. So an item's STATE and its SOURCE are separate fields, and neither
// can be inferred from the other:
//
//   - a check the appliance measured and passed is not the same as
//   - a check the appliance cannot measure here (BLOCKER B2/B3), which is not
//     the same as
//   - a check only a person can perform, which the appliance must never report
//     as done.
//
// There is deliberately no single "READY" boolean. The appliance can answer for
// its own state; it cannot answer for whether a phone in row P can reach the
// patron Wi-Fi, and a composite verdict would let the half it knows vouch for
// the half it does not.

// CheckState is the outcome of one checklist item.
//
// The zero value is CheckNotRun, on purpose: an item nobody filled in reads as
// "not checked" rather than as a pass.
type CheckState string

const (
	// CheckNotRun: nothing has measured this. It is NOT a failure and NOT a
	// pass — it is the absence of an answer, and it blocks a house opening
	// exactly as a failure does when the item is blocking.
	CheckNotRun CheckState = ""
	// CheckPass: measured, and good.
	CheckPass CheckState = "pass"
	// CheckFail: measured, and bad.
	CheckFail CheckState = "fail"
	// CheckUnmeasurable: this appliance CANNOT answer this here, and the reason
	// is structural rather than transient — no hardware, no phone fleet, a
	// blocker in BLOCKERS.md. Distinguished from CheckNotRun because "we have
	// not looked yet" and "nothing on this box can look" lead a booth to do
	// completely different things.
	CheckUnmeasurable CheckState = "unmeasurable"
)

// Label renders a state for a person. Every one of them is spelled out; none is
// abbreviated into a symbol that could be read as a tick.
func (c CheckState) Label() string {
	switch c {
	case CheckPass:
		return "PASS"
	case CheckFail:
		return "FAIL"
	case CheckUnmeasurable:
		return "CANNOT BE CHECKED HERE"
	default:
		return "NOT CHECKED"
	}
}

// CheckSource says who can answer an item.
type CheckSource string

const (
	// SourceMeasured: the appliance observed this itself.
	SourceMeasured CheckSource = "measured"
	// SourceStaff: only a person can answer it — walking to a seat with a
	// phone, looking at an SSID list, checking a receiver drawer. The appliance
	// prints the step and the pass criterion and records NOTHING about whether it
	// was done, because an attestation it cannot verify, stored and shown later,
	// is indistinguishable from a measurement and ages far worse: last week's
	// tick would sit on tonight's page.
	SourceStaff CheckSource = "staff"
)

// PreShowCheck is one line of the checklist.
type PreShowCheck struct {
	// ID is stable and machine-readable, for the UI and for support calls.
	ID string `json:"id"`
	// Title is what a person reads.
	Title string `json:"title"`
	// Section points at the buildplan clause this item comes from, so a booth's
	// question can be traced to a requirement.
	Section string      `json:"section"`
	Source  CheckSource `json:"source"`
	State   CheckState  `json:"state"`
	// Detail is what was actually observed, in plain words. For a staff item it
	// is the pass criterion — what the person is looking for.
	Detail string `json:"detail"`
	// Remedy is §11.5's real requirement: what staff DO when this item is not
	// passing. It is present even on a passing item, because the sheet is read
	// while something is wrong.
	Remedy string `json:"remedy"`
	// Blocking: this item stops the house opening. Non-blocking items are worth
	// knowing and are not worth delaying an audience for.
	Blocking bool `json:"blocking"`
}

// Passing reports whether this item is measured and good.
func (c PreShowCheck) Passing() bool { return c.State == CheckPass }

// PreShowChecklist is the whole sheet.
type PreShowChecklist struct {
	// Auditorium names the room this sheet describes, so a tech with two booth
	// laptops open cannot confuse them (§8.5).
	Auditorium string `json:"auditorium"`
	// GeneratedAt is when these measurements were taken, in UTC. A checklist is
	// a snapshot and says so: one printed an hour ago describes an hour ago.
	GeneratedAt string         `json:"generated_at"`
	Checks      []PreShowCheck `json:"checks"`
	// ApplianceReady: every BLOCKING item the appliance can measure is passing.
	// It is deliberately named for what it covers. It is not "ready for a show",
	// because the appliance cannot see the auditorium.
	ApplianceReady bool `json:"appliance_ready"`
	// BlockingProblems are the ids of blocking items that are failing, could not
	// be checked, or were never run.
	BlockingProblems []string `json:"blocking_problems,omitempty"`
	// StaffStepsOutstanding counts the items only a person can complete. It is
	// always the full count of staff items: the appliance records no
	// attestations, so from its point of view every one of them is outstanding
	// every time the sheet is generated.
	StaffStepsOutstanding int `json:"staff_steps_outstanding"`
}
