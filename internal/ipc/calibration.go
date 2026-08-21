package ipc

// Additive message types for the §6.4 audio calibration wizard. Core owns the
// screen; openaccessbridge-media owns every device, so the wizard's device list and
// its live input meter both have to cross this loopback seam (§3.5).
//
// The payload shapes live in internal/audio/calibration (calibration.Request,
// calibration.Report, calibration.DeviceList) for the same reason
// model.MediaCommandResult does not live here: this package is the transport,
// not the vocabulary.
const (
	// TypeMediaDevices — core → helper: enumerate the attached capture inputs for
	// the §6.4 step-1 device picker. The reply is a calibration.DeviceList, which
	// distinguishes "no sound subsystem" from "no inputs attached" rather than
	// collapsing both into an empty dropdown.
	TypeMediaDevices = "media.devices"
	// TypeMediaCalibrate — core → helper: start (or refresh) a live calibration
	// measurement for one device + mapping (§6.4 steps 4-6). Repeating the same
	// request refreshes the meter; a different one restarts the run. The reply is
	// a calibration.Report, which always carries the provenance of what it
	// measured. It runs BESIDE the configured show streams and never replaces
	// them: calibrating one input must not silence a room mid-show.
	TypeMediaCalibrate = "media.calibrate"
	// TypeMediaCalibrateStop — core → helper: end the calibration run and release
	// the input. The reply is a calibration.Report describing the stopped run.
	TypeMediaCalibrateStop = "media.calibrate.stop"
)
