package web

// The §6.4 audio calibration wizard's screen and its server contract.
//
// §6.4 is a nine-step guided workflow: pick the input device, pick the intended
// use, pick the channel mapping (§6.5), play a known signal, watch a LIVE INPUT
// METER, detect the defects, get a concrete fix, confirm with an explicit
// Excellent/Usable/Marginal/Failed verdict and a plain-language summary, and save
// the auditorium input profile so re-opening the room reuses known-good settings.
//
// The decisions all live in internal/audio/calibration, which is pure and
// testable without a sound card; the measurements all live in
// openaccessbridge-media, which owns every device. This file is the thin layer
// between them and a booth tech's browser: it never measures anything itself and
// never decides a verdict itself.
//
// ADMIN ONLY (§8.4). Every route here is registered on the admin listener behind
// requireAdmin. Device names, dBFS figures, qualifier reasons and mapping tokens
// are diagnostics; none of it may appear on a patron route (§7.2), and none of it
// is logged (§9.4 — the gain notes are operator free text and the device id is
// configuration).
//
// WHY THIS TALKS TO THE MEDIA SOCKET DIRECTLY. The dashboard's audio facts ride
// the supervisor's shared status snapshot, which is polled once for everybody.
// The wizard is the opposite shape: a single operator, an interactive session,
// a meter that has to be current to be worth looking at, and per-session state
// (which device, which mapping) that has no business in a process-wide snapshot.
// So it makes its own short-lived loopback request per meter tick, exactly as
// core's other helper clients do (§3.5). CalibrationDriver keeps that
// substitutable, and tests inject a fake.
//
// HONESTY (the whole point of this screen). This appliance image has no ALSA
// capture source (BLOCKERS B3/B4). openaccessbridge-media therefore refuses to
// measure a named device, and the only source it can run is the synthetic tone.
// Every figure this file hands the browser carries the provenance that produced
// it, and saveProfile will not write §10.5's last_qualification from anything but
// a hardware measurement of the very device being saved. A synthetic verdict
// leaves the input UNQUALIFIED, and says so.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/audio/calibration"
	"github.com/simpledcp/openaccessbridge/internal/audio/capture"
	"github.com/simpledcp/openaccessbridge/internal/audio/mapper"
	"github.com/simpledcp/openaccessbridge/internal/audio/qualifier"
	"github.com/simpledcp/openaccessbridge/internal/ipc"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// CalibrationDriver is the seam to the process that owns audio. Core's web
// surface owns none, so every device fact and every measurement comes from here.
type CalibrationDriver interface {
	// Devices lists the attached capture inputs for §6.4 step 1.
	Devices() (calibration.DeviceList, error)
	// Measure starts or refreshes the live calibration run (§6.4 steps 4-6).
	Measure(calibration.Request) (calibration.Report, error)
	// Stop ends the run and releases the input.
	Stop() (calibration.Report, error)
}

// meterInterval is how often the live meter pushes a frame. The pipeline
// qualifies in 200 ms analysis windows, so anything faster re-sends the same
// verdict; anything much slower stops being a meter.
const meterInterval = 500 * time.Millisecond

// registerCalibrationRoutes registers the §6.4 wizard on an admin mux. It is a
// method on Server so the integrator's hookup is one line wherever the admin
// routes are assembled.
func (s *Server) registerCalibrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/admin/calibrate", s.requireAdmin(s.serveCalibratePage))
	mux.HandleFunc("/api/admin/calibration/devices", s.requireAdmin(s.handleCalibrationDevices))
	mux.HandleFunc("/api/admin/calibration/measure", s.requireAdmin(postOnly(s.handleCalibrationMeasure)))
	mux.HandleFunc("/api/admin/calibration/meter", s.requireAdmin(s.handleCalibrationMeter))
	mux.HandleFunc("/api/admin/calibration/stop", s.requireAdmin(postOnly(s.handleCalibrationStop)))
	mux.HandleFunc("/api/admin/calibration/save", s.requireAdmin(postOnly(s.handleCalibrationSave)))
}

// calibrationSurface composes the §6.4 routes in front of the rest of the admin
// listener, the same way session_admin.go composes the per-show session routes:
// internal/web/admin.go belongs to another lane and adding routes there would
// collide. Behaviourally identical — same listener, same requireAdmin, same
// security headers.
func (s *Server) calibrationSurface() http.Handler {
	mux := http.NewServeMux()
	s.registerCalibrationRoutes(mux)
	mux.Handle("/", s.adminSurface())
	return securityHeaders(mux)
}

func (s *Server) serveCalibratePage(w http.ResponseWriter, r *http.Request) {
	s.serveAsset(w, "assets/calibrate.html")
}

// --- §6.4 steps 1-3: what the pickers may offer ---------------------------

// CalibrationMode is one selectable §6.5 channel mapping.
type CalibrationMode struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// CalibrationDevice is one entry in the §6.4 step-1 picker.
type CalibrationDevice struct {
	ID           string `json:"id"`
	IDKind       string `json:"id_kind"`
	Stable       bool   `json:"stable"`
	FriendlyName string `json:"friendly_name"`
	// Channels is the detected capture channel count; ChannelsKnown is false when
	// the kernel does not expose it without opening the device. 0 means UNKNOWN,
	// never mono (§6.1).
	Channels      int  `json:"channels"`
	ChannelsKnown bool `json:"channels_known"`
	// Synthetic marks the stand-in tone source, which is offered under its own
	// name rather than borrowing a device's identity.
	Synthetic bool `json:"synthetic"`
	// Measurable reports whether THIS BUILD can open the device and measure it.
	// False for real hardware here (BLOCKERS B3/B4): it can still be selected and
	// saved, but the wizard will not pretend to have measured it.
	Measurable bool   `json:"measurable"`
	Detail     string `json:"detail,omitempty"`
	// Modes is the §6.5 mapping list per intended use. A mapping the detected
	// hardware cannot satisfy is not offered at all — an installer must not be
	// able to pick a stereo split on a mono device and discover it mid-show.
	Modes map[string][]CalibrationMode `json:"modes"`
}

// CalibrationUse is one §6.4 step-2 intended use.
type CalibrationUse struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// CalibrationTier is one §6.2 input tier the installer can declare.
type CalibrationTier struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// CalibrationDevicesResponse is everything the three pickers need.
type CalibrationDevicesResponse struct {
	Devices []CalibrationDevice `json:"devices"`
	Uses    []CalibrationUse    `json:"uses"`
	Tiers   []CalibrationTier   `json:"tiers"`
	// EnumerationError is why the device list is empty. "This host has no sound
	// subsystem" and "no inputs are attached" are different faults with the same
	// symptom, and an installer who cannot tell them apart re-seats the wrong
	// cable (§6.3 presence / device health).
	EnumerationError string `json:"enumeration_error,omitempty"`
	// HelperError is set when openaccessbridge-media could not be reached at all.
	HelperError string `json:"helper_error,omitempty"`
	// HardwareCapture reports whether any listed device can actually be measured
	// on this build; HardwareDetail says why not.
	HardwareCapture bool   `json:"hardware_capture"`
	HardwareDetail  string `json:"hardware_detail,omitempty"`
	// DefaultAuditoriumID pre-fills the room the profile will be saved under.
	DefaultAuditoriumID string `json:"default_auditorium_id,omitempty"`
	// SavedProfile is what this room is currently configured with, so re-opening
	// the wizard shows the known-good settings §6.4 step 9 persisted.
	SavedProfile *model.AuditoriumProfile `json:"saved_profile,omitempty"`
}

func (s *Server) handleCalibrationDevices(w http.ResponseWriter, r *http.Request) {
	out := CalibrationDevicesResponse{
		Uses:                calibrationUses(),
		Tiers:               calibrationTiers(),
		DefaultAuditoriumID: s.cfg.AuditoriumID,
	}
	if prof, ok := s.storedProfile(s.cfg.AuditoriumID); ok {
		out.SavedProfile = &prof
	}
	list, err := s.calibrationDriver().Devices()
	if err != nil {
		// Not reachable is not "no devices": say which it is (guardrail #7).
		out.HelperError = "the media process is not answering, so no audio input can be listed or measured"
		writeJSON(w, out)
		return
	}
	out.EnumerationError = list.Error
	out.HardwareCapture = list.HardwareCapture
	out.HardwareDetail = list.HardwareDetail
	if list.SyntheticDeviceID != "" {
		out.Devices = append(out.Devices, describeDevice(capture.Device{
			ID: list.SyntheticDeviceID, FriendlyName: list.SyntheticLabel,
			// The stand-in presents whatever the chosen mapping needs, so it never
			// constrains the §6.5 picker.
			Channels: 2,
		}, true, true, ""))
	}
	for _, d := range list.Devices {
		out.Devices = append(out.Devices, describeDevice(d, false, list.HardwareCapture, list.HardwareDetail))
	}
	writeJSON(w, out)
}

// describeDevice projects a detected device into the picker, attaching the §6.5
// mappings it can actually satisfy for each intended use.
func describeDevice(d capture.Device, synthetic, measurable bool, detail string) CalibrationDevice {
	out := CalibrationDevice{
		ID: d.ID, IDKind: string(d.IDKind), Stable: d.Stable || synthetic,
		FriendlyName: d.FriendlyName, Channels: d.Channels, ChannelsKnown: d.Channels > 0,
		Synthetic: synthetic, Measurable: measurable, Modes: map[string][]CalibrationMode{},
	}
	if !measurable {
		out.Detail = detail
	}
	for _, use := range calibration.Uses() {
		var modes []CalibrationMode
		for _, m := range calibration.ModesFor(d, use) {
			modes = append(modes, CalibrationMode{Value: m.String(), Label: calibration.ModeLabel(m)})
		}
		out.Modes[string(use)] = modes
	}
	return out
}

func calibrationUses() []CalibrationUse {
	out := make([]CalibrationUse, 0, 3)
	for _, u := range calibration.Uses() {
		out = append(out, CalibrationUse{Value: string(u), Label: calibration.UseLabel(u)})
	}
	return out
}

func calibrationTiers() []CalibrationTier {
	tiers := qualifier.Tiers()
	out := make([]CalibrationTier, 0, len(tiers))
	for _, t := range tiers {
		out = append(out, CalibrationTier{Value: t.String(), Label: t.Label()})
	}
	return out
}

// --- §6.4 steps 5-8: the live meter, the defects, the fix, the verdict ----

// CalibrationReport is one frame of the wizard's live state: what was measured,
// WHAT MEASURED IT, and the decision layer's answer over it.
type CalibrationReport struct {
	Running bool `json:"running"`
	// Measured is false when nothing has been measured at all — a refused run, a
	// helper that is not answering, a run that has not started. The screen then
	// renders "not measured", never a verdict or a level (house rule: a value
	// nobody took must never render as a number or a green tick).
	Measured bool `json:"measured"`
	// SourceKind / SourceDetail are the provenance of every figure below.
	SourceKind   string `json:"source_kind"`
	SourceDetail string `json:"source_detail,omitempty"`
	// RequestedDeviceID is what the installer picked; MeasuredDeviceID is what
	// actually produced these numbers. When they differ, the screen says so.
	RequestedDeviceID string `json:"requested_device_id"`
	MeasuredDeviceID  string `json:"measured_device_id,omitempty"`
	ChannelMapping    string `json:"channel_mapping"`
	MeasuredMS        int    `json:"measured_ms"`
	Settled           bool   `json:"settled"`
	// Qualified reports whether this measurement may be persisted as the selected
	// device's §10.5 last_qualification. QualifiedDetail says why not.
	Qualified       bool                         `json:"qualified"`
	QualifiedDetail string                       `json:"qualified_detail,omitempty"`
	Verdict         model.QualifierState         `json:"verdict,omitempty"`
	Feasibility     calibration.Feasibility      `json:"feasibility"`
	Streams         []model.AudioStreamState     `json:"streams,omitempty"`
	Recommendations []calibration.Recommendation `json:"recommendations,omitempty"`
	Summary         *calibration.Summary         `json:"summary,omitempty"`
	// SummaryLines is the §6.4 step-8 block rendered as the spec prints it, so
	// the screen shows the same text a report or a support ticket would carry.
	SummaryLines []string `json:"summary_lines,omitempty"`
	Error        string   `json:"error,omitempty"`
}

func (s *Server) handleCalibrationMeasure(w http.ResponseWriter, r *http.Request) {
	req, sess, err := s.calibrationRequest(decodeCalibrationBody(w, r))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, s.measureOnce(req, sess))
}

func (s *Server) handleCalibrationStop(w http.ResponseWriter, r *http.Request) {
	rep, err := s.calibrationDriver().Stop()
	if err != nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, map[string]string{
			"error": "the media process is not answering, so the calibration run could not be stopped",
		})
		return
	}
	writeJSON(w, CalibrationReport{
		Running: rep.Running, SourceKind: string(rep.SourceKind), Error: rep.Error,
	})
}

// handleCalibrationMeter is §6.4 step 5: the live input meter, pushed over
// Server-Sent Events. SSE is the established live-stream transport on this
// appliance (the patron caption path uses it), it needs no dependency and no
// framework (D4), and EventSource reconnects on its own.
//
// It deliberately does NOT stop the run when the browser goes away: EventSource
// reconnects by itself, and tearing the capture down on every reconnect would
// reset the measurement before any verdict could settle. openaccessbridge-media
// releases the input on its own idle timeout, which is what actually covers a
// closed tab.
func (s *Server) handleCalibrationMeter(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	req, sess, err := s.calibrationRequest(calibrationBodyFromQuery(r))
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	tick := time.NewTicker(meterInterval)
	defer tick.Stop()
	for {
		b, err := json.Marshal(s.measureOnce(req, sess))
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return
		}
		flusher.Flush()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// measureOnce refreshes the run and turns the helper's report into the screen's
// frame. It is the ONLY place a verdict is attached to a measurement, so the
// provenance rule has exactly one enforcement point.
func (s *Server) measureOnce(req calibration.Request, sess calibration.Session) CalibrationReport {
	rep, err := s.calibrationDriver().Measure(req)
	if err != nil {
		return CalibrationReport{
			SourceKind:        string(calibration.SourceNone),
			RequestedDeviceID: req.DeviceID,
			ChannelMapping:    req.Mode.String(),
			Feasibility:       sess.Feasibility(),
			Error:             "the media process is not answering, so this input is not being measured",
		}
	}
	out := CalibrationReport{
		Running:           rep.Running,
		SourceKind:        string(rep.SourceKind),
		SourceDetail:      rep.SourceDetail,
		RequestedDeviceID: req.DeviceID,
		MeasuredDeviceID:  rep.DeviceID,
		ChannelMapping:    req.Mode.String(),
		MeasuredMS:        rep.MeasuredMS,
		Settled:           rep.Settled(),
		Feasibility:       sess.Feasibility(),
		Error:             rep.Error,
	}
	out.Qualified, out.QualifiedDetail = qualificationOf(rep)
	if len(rep.Streams) == 0 {
		// Nothing was measured. No verdict, no summary, no level — the screen says
		// "not measured" rather than showing a Failed tick for a run that never
		// took a reading.
		return out
	}
	res := calibration.Judge(sess, rep.Streams)
	sum := res.Summary
	out.Measured = true
	out.Verdict = res.Verdict
	out.Streams = res.Streams
	out.Recommendations = res.Recommendations
	out.Summary = &sum
	out.SummaryLines = sum.Lines()
	return out
}

// qualificationOf answers whether a measurement may become a device's §10.5
// last_qualification, and gives the plain-language reason when it may not. The
// DECISION is calibration.Report.Qualified() and only that: this function owns
// the wording, never the rule. Deriving the rule a second time here is how the
// two drift apart and the weaker one wins — which is how a synthetic tone was
// persisted under a real device's identity once already.
func qualificationOf(rep calibration.Report) (bool, string) {
	if rep.Qualified() {
		return true, ""
	}
	switch {
	case rep.SourceKind == calibration.SourceSynthetic:
		return false, "These figures come from the synthetic test source, not from an audio input. " +
			"The settings can be saved, but this input stays UNQUALIFIED until it is measured on real hardware."
	case rep.SourceKind != calibration.SourceHardware || len(rep.Streams) == 0:
		return false, "Nothing has been measured yet, so there is no verdict to record."
	case rep.DeviceID != rep.RequestedDeviceID:
		return false, "The measurement came from a different input than the one selected, so it cannot qualify this one."
	case !rep.Settled():
		return false, fmt.Sprintf("Keep the test signal playing: a verdict needs at least %d ms of audio "+
			"before it describes the input rather than the moment the capture opened.", calibration.MinMeasureMS)
	default:
		// Qualified() refused for a reason this wording does not cover yet. Say
		// so plainly rather than inventing a reassurance: an unexplained refusal
		// is still a refusal, and it must not read as a pass.
		return false, "This measurement does not meet the bar for recording a verdict against this input."
	}
}

// --- §6.4 step 9: save the auditorium input profile -----------------------

// CalibrationSaveRequest is the wizard's step-9 submission.
type CalibrationSaveRequest struct {
	AuditoriumID   string `json:"auditorium_id"`
	DeviceID       string `json:"device_id"`
	ChannelMapping string `json:"channel_mapping"`
	Use            string `json:"use"`
	InputTier      string `json:"input_tier"`
	// GainNotes is §10.5's free-text "attenuator pad inline; isolator fitted".
	// It is operator input, so it is persisted but never logged (§9.4).
	GainNotes string `json:"gain_notes"`
	// PartOfPairing marks this as one half of §6.5's "Multiple USB inputs:
	// Device A = HI, Device B = VI-N".
	//
	// The wizard measures ONE input at a time and refuses to run the multi-device
	// plan against a single device, which is correct — it would report both feeds
	// as measured when only one was opened. So a pairing is built by calibrating
	// each device on its own, with a single-stream mapping, and this flag says
	// "keep the other stream's device rather than replacing the room's whole
	// configuration with mine". Without it the second save silently overwrote the
	// first and the pairing was lost.
	PartOfPairing bool `json:"part_of_pairing"`
}

// CalibrationSaveResponse says exactly what was written and what was not.
type CalibrationSaveResponse struct {
	Saved   bool                    `json:"saved"`
	Profile model.AuditoriumProfile `json:"profile"`
	// Qualified reports whether last_qualification was written from a real
	// measurement. When false the field is left EMPTY — an unqualified input,
	// never a verdict nobody took.
	Qualified bool `json:"qualified"`
	// Note is the plain-language statement of what was persisted, shown verbatim.
	Note  string `json:"note,omitempty"`
	Error string `json:"error,omitempty"`
}

// applyPairingHalf records one device of a §6.5 multi-device pairing, keeping
// whatever the other half already had.
//
// It insists on a SINGLE-STREAM mapping. A pairing half calibrated with a stereo
// split would have measured two streams from one device and then claimed to be
// half of a two-device room — a configuration that describes neither of the two
// things it might mean.
func applyPairingHalf(prof *model.AuditoriumProfile, mode mapper.Mode, deviceID string) error {
	plan, err := mapper.PlanFor(mode, 1)
	if err != nil || len(plan.Maps) != 1 {
		if plan2, err2 := mapper.PlanFor(mode, 2); err2 == nil && len(plan2.Maps) == 1 {
			plan = plan2
		} else {
			return fmt.Errorf("a multi-device pairing is built one stream at a time: %q feeds "+
				"more than one stream, so calibrate this device as HI only or VI-N only (§6.5)", mode)
		}
	}
	stream := plan.Maps[0].Stream
	if prof.StreamDevices == nil {
		prof.StreamDevices = map[model.StreamType]string{}
	}
	prof.StreamDevices[stream] = deviceID
	prof.ChannelMapping = mapper.MultiDevice.String()
	// InputDeviceID stays as the first device calibrated, so a reader that knows
	// nothing about pairings still has something real rather than an empty field.
	if strings.TrimSpace(prof.InputDeviceID) == "" {
		prof.InputDeviceID = deviceID
	}
	return nil
}

// pairingNote says what is still missing, so an operator knows the room is not
// finished. Silence here is how the first version lost the second device.
func pairingNote(prof model.AuditoriumProfile) string {
	var missing []string
	for _, st := range []model.StreamType{model.StreamHI, model.StreamVIN} {
		if strings.TrimSpace(prof.StreamDevices[st]) == "" {
			missing = append(missing, string(st))
		}
	}
	if len(missing) == 0 {
		return "Both halves of this multi-device room are now assigned."
	}
	return "This room is NOT finished: " + strings.Join(missing, " and ") +
		" still has no input device. Calibrate the second device and save it as part of the pairing."
}

func (s *Server) handleCalibrationSave(w http.ResponseWriter, r *http.Request) {
	var body CalibrationSaveRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
	}
	if s.deps.Store == nil {
		writeJSONStatus(w, http.StatusServiceUnavailable, CalibrationSaveResponse{
			Error: "no configuration store is attached, so this profile cannot be persisted",
		})
		return
	}
	req, sess, err := s.calibrationRequest(body)
	if err != nil {
		writeJSONStatus(w, http.StatusBadRequest, CalibrationSaveResponse{Error: err.Error()})
		return
	}
	// §6.5: a mapping the detected hardware cannot satisfy is refused HERE too,
	// not only hidden from the picker. The rule is "surfaced before a show", and
	// a hand-built request must not be able to route around it.
	if f := sess.Feasibility(); !f.OK && !f.Unknown {
		writeJSONStatus(w, http.StatusBadRequest, CalibrationSaveResponse{
			Error: "this mapping cannot be satisfied by the selected input: " + f.Reason,
		})
		return
	}
	auditorium := strings.TrimSpace(body.AuditoriumID)
	if auditorium == "" {
		auditorium = s.cfg.AuditoriumID
	}
	if auditorium == "" {
		writeJSONStatus(w, http.StatusBadRequest, CalibrationSaveResponse{
			Error: "an auditorium id is required so the profile can be found again when the room re-opens",
		})
		return
	}

	// The verdict saved is the verdict of the run that is measuring THIS mapping
	// right now: refreshing rather than trusting a report the browser sent up.
	rep, derr := s.calibrationDriver().Measure(req)
	qualified, why := false, "The media process is not answering, so nothing could be measured."
	if derr == nil {
		qualified, why = qualificationOf(rep)
	}

	prof, _ := s.storedProfile(auditorium)
	prof.AuditoriumID = auditorium
	prof.GainNotes = strings.TrimSpace(body.GainNotes)
	if body.PartOfPairing {
		if err := applyPairingHalf(&prof, req.Mode, req.DeviceID); err != nil {
			writeJSONStatus(w, http.StatusBadRequest, CalibrationSaveResponse{Error: err.Error()})
			return
		}
	} else {
		// A single-device room replaces the whole assignment, pairing included:
		// leaving a stale per-stream device behind would mean the room ran on a
		// mapping nobody chose.
		prof.InputDeviceID = req.DeviceID
		prof.ChannelMapping = req.Mode.String()
		prof.StreamDevices = nil
	}
	out := CalibrationSaveResponse{Qualified: qualified}
	if qualified {
		prof.LastQualification = calibration.Judge(sess, rep.Streams).Verdict
		out.Note = "Saved. This input measured " + string(prof.LastQualification) + " on real hardware."
	} else {
		// UNQUALIFIED, and stored that way. An empty last_qualification is the
		// honest record of an input nobody measured; writing the synthetic run's
		// verdict here is the exact fabrication this lane exists to prevent.
		prof.LastQualification = ""
		out.Note = "Settings saved. No verdict was recorded for this input. " + why
	}
	if body.PartOfPairing {
		out.Note += " " + pairingNote(prof)
	}
	if err := s.deps.Store.PutAuditoriumProfile(prof); err != nil {
		writeJSONStatus(w, http.StatusInternalServerError, CalibrationSaveResponse{
			Error: "the auditorium profile could not be written to the configuration store",
		})
		return
	}
	// Audited without content: no device id, no mapping token, no gain notes
	// (§9.4 — operator free text and configuration identifiers stay out of logs).
	s.deps.Log.Info(logging.EvStateTransition,
		logging.Field("phase", "auditorium_input_profile_saved"),
		logging.Field("qualified", qualified))
	out.Saved = true
	out.Profile = prof
	writeJSON(w, out)
}

// --- request plumbing -----------------------------------------------------

// calibrationRequest validates the installer's selections and builds both the
// helper request and the decision layer's Session. An unrecognized mapping,
// intended use or tier is an ERROR rather than a silent fallback: guessing which
// accessibility feed or which hardware tier was meant is how a room ends up
// serving the wrong thing (guardrail #7).
func (s *Server) calibrationRequest(body CalibrationSaveRequest) (calibration.Request, calibration.Session, error) {
	mode, err := mapper.ParseMode(body.ChannelMapping)
	if err != nil {
		return calibration.Request{}, calibration.Session{}, errors.New("channel mapping not recognized (§6.5)")
	}
	use, err := calibration.ParseUse(body.Use)
	if err != nil {
		return calibration.Request{}, calibration.Session{}, errors.New("intended use not recognized (§6.4 step 2: HI, VI-N or dual)")
	}
	tier, err := qualifier.ParseTier(strings.TrimSpace(body.InputTier))
	if err != nil {
		return calibration.Request{}, calibration.Session{}, errors.New("input tier not recognized (§6.2)")
	}
	dev := s.deviceByID(body.DeviceID)
	req := calibration.Request{DeviceID: dev.ID, Mode: mode, Use: use, Tier: tier}
	sess := calibration.Session{Device: dev, Use: use, Mode: mode, Tier: tier, GainNotes: body.GainNotes}
	return req, sess, nil
}

// deviceByID resolves the picked device back to its detected record, because
// Feasibility (§6.5) and the stable-identifier advisory (§6.1) are properties of
// the DEVICE, not of the id string. A device that can no longer be enumerated is
// returned with an unknown channel count rather than an invented one — the
// wizard then says "unknown" instead of guessing mono or stereo.
func (s *Server) deviceByID(id string) capture.Device {
	id = strings.TrimSpace(id)
	list, err := s.calibrationDriver().Devices()
	if err == nil {
		for _, d := range list.Devices {
			if d.ID == id {
				return d
			}
		}
		if id != "" && id == list.SyntheticDeviceID {
			// The stand-in presents whatever the mapping asks for, so it never
			// makes a mapping infeasible; it is not real hardware either way.
			return capture.Device{ID: id, FriendlyName: list.SyntheticLabel, Channels: 2, Stable: true}
		}
	}
	return capture.Device{ID: id}
}

// storedProfile reads the §10.5 profile for a room, so a save updates it in place
// rather than dropping the cinema-server identity and wrong-room guards next to it.
func (s *Server) storedProfile(id string) (model.AuditoriumProfile, bool) {
	if s.deps.Store == nil || id == "" {
		return model.AuditoriumProfile{}, false
	}
	p, ok, err := s.deps.Store.GetAuditoriumProfile(id)
	if err != nil || !ok {
		return model.AuditoriumProfile{}, false
	}
	return p, true
}

func decodeCalibrationBody(w http.ResponseWriter, r *http.Request) CalibrationSaveRequest {
	var body CalibrationSaveRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
	}
	return body
}

// calibrationBodyFromQuery reads the same selections off an EventSource URL,
// which cannot carry a request body.
func calibrationBodyFromQuery(r *http.Request) CalibrationSaveRequest {
	q := r.URL.Query()
	return CalibrationSaveRequest{
		DeviceID:       q.Get("device_id"),
		ChannelMapping: q.Get("channel_mapping"),
		Use:            q.Get("use"),
		InputTier:      q.Get("input_tier"),
	}
}

func (s *Server) calibrationDriver() CalibrationDriver {
	if s.deps.Calibration != nil {
		return s.deps.Calibration
	}
	return unreachableDriver{}
}

// unreachableDriver is what the wizard gets when no driver was configured. It
// fails LOUDLY on every call rather than returning empty lists that would read
// as "no devices attached" (guardrail #7).
type unreachableDriver struct{}

var errNoMediaHelper = errors.New("no media process is configured for this appliance")

func (unreachableDriver) Devices() (calibration.DeviceList, error) {
	return calibration.DeviceList{}, errNoMediaHelper
}
func (unreachableDriver) Measure(calibration.Request) (calibration.Report, error) {
	return calibration.Report{}, errNoMediaHelper
}
func (unreachableDriver) Stop() (calibration.Report, error) {
	return calibration.Report{}, errNoMediaHelper
}

// --- the loopback driver --------------------------------------------------

// mediaCalibrationDriver talks to openaccessbridge-media over its loopback Unix
// socket (§3.5). It dials per request, exactly as core's other helper clients do:
// a wedged helper then fails this one request instead of holding a connection
// that outlives it.
type mediaCalibrationDriver struct{ socket string }

// NewMediaCalibrationDriver builds the §6.4 wizard's driver for a media socket
// path. Exported so the core process can inject the socket it actually supervises
// rather than the default one.
func NewMediaCalibrationDriver(socketPath string) CalibrationDriver {
	return &mediaCalibrationDriver{socket: socketPath}
}

// defaultMediaSocket is where openaccessbridge-media listens when nobody says
// otherwise — the same default cmd/openaccessbridge-core's --socket-dir
// carries.
func defaultMediaSocket(dir string) string {
	if dir == "" {
		dir = ipc.DefaultSocketDir()
	}
	return filepath.Join(dir, ipc.SocketMedia)
}

func (d *mediaCalibrationDriver) request(msg ipc.Message, out any) error {
	cli, err := ipc.Dial(d.socket)
	if err != nil {
		return err
	}
	defer cli.Close()
	resp, err := cli.Request(msg)
	if err != nil {
		return err
	}
	if len(resp.Payload) == 0 {
		return errors.New("media process returned an empty reply")
	}
	return json.Unmarshal(resp.Payload, out)
}

func (d *mediaCalibrationDriver) Devices() (calibration.DeviceList, error) {
	var out calibration.DeviceList
	err := d.request(ipc.Message{Type: ipc.TypeMediaDevices, ID: "devices"}, &out)
	return out, err
}

func (d *mediaCalibrationDriver) Measure(req calibration.Request) (calibration.Report, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return calibration.Report{}, err
	}
	var out calibration.Report
	err = d.request(ipc.Message{Type: ipc.TypeMediaCalibrate, ID: "calibrate", Payload: payload}, &out)
	return out, err
}

func (d *mediaCalibrationDriver) Stop() (calibration.Report, error) {
	var out calibration.Report
	err := d.request(ipc.Message{Type: ipc.TypeMediaCalibrateStop, ID: "calibrate-stop"}, &out)
	return out, err
}
