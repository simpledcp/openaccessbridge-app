package deploy

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

func TestBuiltinProfilesPosture(t *testing.T) {
	b := Builtin()
	if len(b) != 4 {
		t.Fatalf("expected 4 builtin profiles, got %d", len(b))
	}

	// Production profiles must hide admin from patron, disable SSH, and require
	// per-show tokens (§8.4, §9.2).
	for _, name := range []model.FirewallProfile{model.ProfilePatronFacing, model.ProfileLowCostReplacement} {
		p := b[name]
		if !p.Name.Production() {
			t.Errorf("%s should be a production profile", name)
		}
		if !p.AdminHiddenFromPatron || !p.SSHDisabled || !p.PerShowToken || !p.TheaterAllowlistOnly {
			t.Errorf("%s lacks a required production posture: %+v", name, p)
		}
	}

	// Lab allows packet capture + local SSH + a warning banner; never for public.
	lab := b[model.ProfileLab]
	if !lab.AllowPacketCapture || !lab.AllowLocalSSH || !lab.WarningBanner {
		t.Errorf("lab posture wrong: %+v", lab)
	}

	// Low-Cost Replacement adds mandatory calibration + signal health and requires
	// the Phase 9 review (§12.4 / §1.3).
	lc := b[model.ProfileLowCostReplacement]
	if !lc.RequireCalibration || !lc.RequireSignalHealth || !lc.RequiresPhase9Review {
		t.Errorf("low-cost replacement posture wrong: %+v", lc)
	}
	if lc.MaxConcurrentClients != 100 {
		t.Errorf("low-cost client cap = %d, want 100", lc.MaxConcurrentClients)
	}

	// Booth Diagnostic makes no access-equivalence claim and needs no Phase 9 gate.
	bd := b[model.ProfileBoothDiagnostic]
	if bd.RequiresPhase9Review {
		t.Errorf("booth diagnostic should not require Phase 9 review")
	}
}

func TestLoanerOverlay(t *testing.T) {
	o := LoanerOverlay{Enabled: true, PreTrustedCert: true, SuppliedWiredHeadphones: true, KioskLock: true}
	if !o.ApplicableTo(model.ProfilePatronFacing) || !o.ApplicableTo(model.ProfileLowCostReplacement) {
		t.Errorf("loaner overlay should apply to patron-facing and low-cost replacement")
	}
	if o.ApplicableTo(model.ProfileLab) || o.ApplicableTo(model.ProfileBoothDiagnostic) {
		t.Errorf("loaner overlay must not apply to lab/booth-diagnostic")
	}
	base, _ := Get(model.ProfilePatronFacing)
	got := o.Apply(base)
	if !got.RequireClearErrorStates || !got.PurgeAtShowEnd {
		t.Errorf("loaner overlay should strengthen access posture: %+v", got)
	}

	// Disabled overlay is a no-op.
	if (LoanerOverlay{}).Apply(base) != base {
		t.Errorf("disabled overlay should be a no-op")
	}
}

// An un-provisioned or unrecognized profile must resolve to the safe default
// (Booth Diagnostic, D13 / §12.2) — never to Lab, which is the most permissive
// of the four and would give a fresh appliance admin-on-patron-net, local SSH,
// and packet capture (§9.2).
func TestResolveFallsBackToSafeDefault(t *testing.T) {
	if DefaultProfile == model.ProfileLab {
		t.Fatal("the default posture must not be Lab")
	}
	if DefaultProfile != model.ProfileBoothDiagnostic {
		t.Errorf("DefaultProfile = %q, want booth_diagnostic (D13)", DefaultProfile)
	}

	for _, name := range []model.FirewallProfile{"", "lab_but_typoed", "LAB", "patron facing"} {
		got, known := Resolve(name)
		if known {
			t.Errorf("Resolve(%q) reported the name as known", name)
		}
		if got.Name != DefaultProfile {
			t.Errorf("Resolve(%q) = %q, want the safe default %q", name, got.Name, DefaultProfile)
		}
		if !got.AdminHiddenFromPatron && !got.WarningBanner {
			t.Errorf("Resolve(%q) fell back to a posture that neither hides admin nor warns", name)
		}
	}

	// A recognized name still resolves to itself, including Lab when it is the
	// operator's explicit choice.
	for _, name := range []model.FirewallProfile{
		model.ProfileLab, model.ProfileBoothDiagnostic,
		model.ProfilePatronFacing, model.ProfileLowCostReplacement,
	} {
		got, known := Resolve(name)
		if !known || got.Name != name {
			t.Errorf("Resolve(%q) = (%q, %v), want the profile itself", name, got.Name, known)
		}
	}
}

// §8.4: a profile that leaves the admin surface reachable from the patron side
// must carry the prominent warning banner. The patron network is untrusted BYOD;
// an un-flagged admin surface on it is exactly the configuration the banner
// exists to make impossible to miss.
func TestAdminOnPatronNetAlwaysWarns(t *testing.T) {
	for name, p := range Builtin() {
		if !p.AdminHiddenFromPatron && !p.WarningBanner {
			t.Errorf("profile %q exposes admin to the patron side with no warning banner (§8.4)", name)
		}
		// The converse: production profiles must hide admin outright, banner or not.
		if name.Production() && !p.AdminHiddenFromPatron {
			t.Errorf("production profile %q must hide admin from the patron network (§8.4)", name)
		}
	}
}
