package readiness

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

func lowCost(d Deploy) Deploy {
	d.Profile = model.ProfileLowCostReplacement
	d.RequireCalibration = true
	d.RequireSignalHealth = true
	d.RequiresPhase9Review = true
	return d
}

// §12.4's requirements were flags on the deployment profile that NOTHING read.
// An operator could select the profile for venues running CaptionBridge in lieu
// of a professional receiver system with no calibration ever run, nothing being
// monitored, and no Phase 9 review.
func TestTheReplacementProfileRefusesItsOwnUnmetRequirements(t *testing.T) {
	r := Assemble(Inputs{Verify: cleanVerify(), Deploy: lowCost(Deploy{})})

	for _, code := range []string{
		model.WarnCalibrationRequired,
		model.WarnSignalHealthRequired,
		model.WarnPhase9ReviewMissing,
	} {
		w := findWarning(r.Warnings, code)
		if w == nil {
			t.Errorf("the profile requires %s and nothing raised it", code)
			continue
		}
		// Critical, because internal/web's setup gate blocks on critical: the same
		// condition must stop an installer declaring setup complete.
		if w.Severity != model.SeverityCritical {
			t.Errorf("%s is %q; it must block", code, w.Severity)
		}
	}
}

// With the requirements met, the profile is quiet — so the test above is not
// simply asserting that this profile always complains.
func TestAFullyProvisionedReplacementProfileIsQuiet(t *testing.T) {
	r := Assemble(Inputs{
		Verify: cleanVerify(),
		Deploy: lowCost(Deploy{CalibrationRecorded: true, Phase9ReviewRecorded: true}),
		Audio: []model.AudioStreamState{
			{StreamType: model.StreamHI, Present: true, Status: model.QualExcellent, DeviceConnected: true},
		},
	})
	for _, code := range []string{
		model.WarnCalibrationRequired,
		model.WarnSignalHealthRequired,
		model.WarnPhase9ReviewMissing,
	} {
		if w := findWarning(r.Warnings, code); w != nil {
			t.Errorf("a provisioned appliance still raises %s: %s", code, w.Message)
		}
	}
}

// The requirements belong to the PROFILE. A diagnostic appliance must not be
// blocked by rules §12.4 puts on a replacement deployment.
func TestOtherProfilesAreNotHeldToTheReplacementRequirements(t *testing.T) {
	r := Assemble(Inputs{
		Verify: cleanVerify(),
		Deploy: Deploy{Profile: model.ProfileBoothDiagnostic},
	})
	for _, code := range []string{
		model.WarnCalibrationRequired,
		model.WarnSignalHealthRequired,
		model.WarnPhase9ReviewMissing,
	} {
		if w := findWarning(r.Warnings, code); w != nil {
			t.Errorf("Booth Diagnostic was held to %s", code)
		}
	}
}
