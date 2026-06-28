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
