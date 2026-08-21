package readiness

import (
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
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
