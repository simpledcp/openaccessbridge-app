package readiness

import (
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// D27's condition, on the dashboard: patrons were handed candidates for more
// than one of this appliance's networks. Every extra one is join time a phone
// spends timing out, and it publishes the venue's private topology to the patron
// LAN.
func TestOfferingPatronsMoreThanOneNetworkIsAWarning(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), ICEScope: model.ICEScopeReport{
		Answered:          true,
		OfferedInterfaces: []string{"docker0", "eno1", "tailscale0"},
		OfferedCandidates: 12,
	}})
	w := findWarning(r.Warnings, model.WarnICEScopeUnconfined)
	if w == nil {
		t.Fatalf("three networks offered to patrons and no warning: %+v", r.Warnings)
	}
	// The booth has to be able to act on it, so the message names them.
	for _, iface := range []string{"docker0", "eno1", "tailscale0"} {
		if !strings.Contains(w.Message, iface) {
			t.Errorf("the warning does not name %q: %q", iface, w.Message)
		}
	}
}

// The warning is raised from what was MEASURED, so an appliance nobody has
// joined yet raises nothing: there is no answer to have offered anything, and a
// warning about an event that never happened is the same fabrication as a
// green tick for a check that never ran.
func TestAnApplianceNobodyHasJoinedRaisesNoScopeWarning(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
	}})
	if w := findWarning(r.Warnings, model.WarnICEScopeUnconfined); w != nil {
		t.Fatalf("nobody has joined yet and the dashboard already judges the offer: %+v", w)
	}
}

// A confined appliance is quiet.
func TestOneOfferedNetworkRaisesNothing(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
		Answered: true, OfferedInterfaces: []string{"wlan0"}, OfferedCandidates: 2,
	}})
	if w := findWarning(r.Warnings, model.WarnICEScopeUnconfined); w != nil {
		t.Fatalf("a confined appliance raised %+v", w)
	}
}

// The report reaches the dashboard rows, not only the warning set: §8.2's
// interface rows are where an operator looks to see which network the patron
// side is on, and "configured" and "offered" are different facts.
func TestTheScopeReachesTheDashboardBesideTheConfiguredInterface(t *testing.T) {
	sc := model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
		Answered: true, OfferedInterfaces: []string{"wlan0"}, OfferedCandidates: 2,
	}
	r := Assemble(Inputs{Verify: cleanVerify(), Network: Network{ClientInterface: "wlan0"}, ICEScope: sc})
	if r.ICEScope.Interface != "wlan0" || !r.ICEScope.InForce || r.ICEScope.OfferedCandidates != 2 {
		t.Fatalf("the scope did not reach the dashboard intact: %+v", r.ICEScope)
	}
}

func findWarning(ws []model.Warning, code string) *model.Warning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

// The comparison this pair of fields exists to make. An appliance confined to
// one interface on paper, offering a different one in fact, is a bug — and it
// used to be silent, because one interface is "fewer than two".
func TestConfinedOnPaperButOfferingSomethingElseIsCritical(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
		Answered: true, OfferedInterfaces: []string{"eth0"}, OfferedCandidates: 2,
	}})
	w := findWarning(r.Warnings, model.WarnICEScopeUnconfined)
	if w == nil {
		t.Fatal("the appliance is confined to wlan0 and offering eth0, and nothing was raised")
	}
	if w.Severity != model.SeverityCritical {
		t.Fatalf("severity %q for a confinement that is not doing what it says", w.Severity)
	}
}

// An answer that carried nothing at all is the worst outcome on this path: the
// patron gets a spinner and nobody is told why. It must be loud.
func TestAnAnswerWithNoCandidatesAtAllIsCritical(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
		Answered: true, OfferedInterfaces: nil, OfferedCandidates: 0,
	}})
	w := findWarning(r.Warnings, model.WarnICEScopeUnconfined)
	if w == nil {
		t.Fatal("a patron was offered nothing and the dashboard said nothing")
	}
	if w.Severity != model.SeverityCritical {
		t.Fatalf("severity %q for a patron who could not connect at all", w.Severity)
	}
}

// The §11.5 item is labelled "measured", so it must read the measurement.
func TestTheChecklistScopeItemReadsTheMeasurementNotTheIntent(t *testing.T) {
	facts := ChecklistFacts{Auditorium: "Auditorium 3", Now: func() time.Time { return time.Unix(0, 0) }}

	mismatch := BuildChecklist(model.SystemReadiness{ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
		Answered: true, OfferedInterfaces: []string{"eth0"}, OfferedCandidates: 2,
	}}, facts)
	if c := findCheck(t, mismatch, "patron_network_scoped"); c.State != model.CheckFail {
		t.Fatalf("confined to wlan0, offering eth0, reports %q", c.State)
	}

	none := BuildChecklist(model.SystemReadiness{ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true, Answered: true, OfferedCandidates: 0,
	}}, facts)
	if c := findCheck(t, none, "patron_network_scoped"); c.State != model.CheckFail || !c.Blocking {
		t.Fatalf("a patron offered nothing reports %q blocking=%v", c.State, c.Blocking)
	}

	good := BuildChecklist(model.SystemReadiness{ICEScope: model.ICEScopeReport{
		Configured: true, Interface: "wlan0", InForce: true,
		Answered: true, OfferedInterfaces: []string{"wlan0"}, OfferedCandidates: 2,
	}}, facts)
	if c := findCheck(t, good, "patron_network_scoped"); c.State != model.CheckPass {
		t.Fatalf("a genuinely confined appliance reports %q", c.State)
	}
}

// §11.5 names this item "within the §6.7 budget", so a measured figure must be
// judged against that budget. Anything above zero passing would print PASS beside
// a four-second latency.
func TestAMeasuredLatencyIsJudgedAgainstTheBudget(t *testing.T) {
	facts := ChecklistFacts{Auditorium: "A", Now: func() time.Time { return time.Unix(0, 0) }}
	over := BuildChecklist(model.SystemReadiness{AudioStreams: []model.AudioStreamSummary{
		{StreamType: model.StreamHI, LatencyMeasured: true, LatencyMS: 4000},
	}}, facts)
	if c := findCheck(t, over, "audio_latency_within_budget"); c.State != model.CheckFail {
		t.Fatalf("4000 ms against a %d ms budget reports %q", DefaultLatencyBudgets().HIMaxMS, c.State)
	}
	under := BuildChecklist(model.SystemReadiness{AudioStreams: []model.AudioStreamSummary{
		{StreamType: model.StreamHI, LatencyMeasured: true, LatencyMS: 90},
	}}, facts)
	if c := findCheck(t, under, "audio_latency_within_budget"); c.State != model.CheckPass {
		t.Fatalf("90 ms reports %q", c.State)
	}
}

func findCheck(t *testing.T, cl model.PreShowChecklist, id string) model.PreShowCheck {
	t.Helper()
	for _, c := range cl.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no item %q", id)
	return model.PreShowCheck{}
}
