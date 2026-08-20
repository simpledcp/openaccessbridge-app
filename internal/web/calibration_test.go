package web

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/audio/calibration"
	"github.com/simpledcp/captionbridge/internal/audio/capture"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/model"
)

// fakeCalDriver stands in for captionbridge-media so the wizard's contract can be
// exercised without a media process. It reports exactly what it is told to, which
// is what lets a test hand the web layer the WORST case (a synthetic measurement
// wearing a real device's id) and check the layer refuses it anyway.
type fakeCalDriver struct {
	list       calibration.DeviceList
	kind       calibration.SourceKind
	measuredID string // "" = whatever was requested
	streams    []model.AudioStreamState
	measuredMS int
	detail     string
	errText    string
	fail       error

	last  calibration.Request
	stops int
}

func (f *fakeCalDriver) Devices() (calibration.DeviceList, error) {
	if f.fail != nil {
		return calibration.DeviceList{}, f.fail
	}
	return f.list, nil
}

func (f *fakeCalDriver) Measure(req calibration.Request) (calibration.Report, error) {
	f.last = req
	if f.fail != nil {
		return calibration.Report{}, f.fail
	}
	id := f.measuredID
	if id == "" {
		id = req.DeviceID
	}
	kind := f.kind
	if kind == "" {
		kind = calibration.SourceNone
	}
	return calibration.Report{
		Running: len(f.streams) > 0, SourceKind: kind, SourceDetail: f.detail,
		RequestedDeviceID: req.DeviceID, DeviceID: id, Mode: req.Mode,
		Streams: f.streams, MeasuredMS: f.measuredMS, Error: f.errText,
	}, nil
}

func (f *fakeCalDriver) Stop() (calibration.Report, error) {
	f.stops++
	return calibration.Report{SourceKind: calibration.SourceNone}, f.fail
}

func measuredStream(s model.StreamType, src model.SourceChannel, dev string) model.AudioStreamState {
	return model.AudioStreamState{
		InputDeviceID: dev, StreamType: s, SourceChannel: src,
		SampleRate: 48000, Channels: 2, LevelDBFS: -18, NoiseFloorDBFS: -72,
		Status: model.QualUsable, Present: true, DeviceConnected: true,
	}
}

const testScarlett = "by-id:usb-Focusrite_Scarlett_Solo-00,0"

func newCalibrationServer(t *testing.T, drv CalibrationDriver) (*Server, config.Store) {
	t.Helper()
	store, err := config.OpenSQLite(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	s, err := NewServer(Config{
		Profile: model.ProfileLab, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
		Auditorium: "Auditorium 3", AuditoriumID: "screen-3",
	}, Deps{Hub: fanout.NewHub(fanout.Options{}), Store: store, Calibration: drv})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return s, store
}

func postJSON(t *testing.T, ts *httptest.Server, path string, body any) (*http.Response, []byte) {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp, buf.Bytes()
}

// THE FABRICATION THIS LANE EXISTS TO PREVENT. This appliance already persisted a
// synthetic tone's verdict under a real hardware device's identity once, as
// §10.5's last_qualification, and it looked green in the test suite. Even handed
// the worst possible report — a SYNTHETIC measurement already wearing the
// selected device's id — the save must leave the input UNQUALIFIED and say so.
func TestSaveLeavesARealInputUnqualifiedAfterASyntheticRun(t *testing.T) {
	drv := &fakeCalDriver{
		list: calibration.DeviceList{Devices: []capture.Device{{
			ID: testScarlett, IDKind: capture.DeviceIDByID, Stable: true,
			FriendlyName: "Scarlett Solo — Analog", Channels: 2,
		}}},
		kind:       calibration.SourceSynthetic,
		streams:    []model.AudioStreamState{measuredStream(model.StreamHI, model.ChannelMono, testScarlett)},
		measuredMS: 9000,
		detail:     "the synthetic tone source stood in for a booth feed",
	}
	s, store := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	resp, body := postJSON(t, ts, "/api/admin/calibration/save", CalibrationSaveRequest{
		AuditoriumID: "screen-3", DeviceID: testScarlett,
		ChannelMapping: "mono:HI", Use: "HI", InputTier: "tier2_usb_interface",
		GainNotes: "attenuator pad inline",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("save status = %d: %s", resp.StatusCode, body)
	}
	var out CalibrationSaveResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Qualified {
		t.Fatal("a synthetic measurement must never qualify a real input device")
	}
	if out.Profile.LastQualification != "" {
		t.Fatalf("last_qualification = %q; a synthetic run must leave it EMPTY (unqualified)",
			out.Profile.LastQualification)
	}
	if !strings.Contains(strings.ToUpper(out.Note), "UNQUALIFIED") {
		t.Errorf("note = %q; it must tell the operator the input is unqualified", out.Note)
	}

	// And the same thing must be true of what actually landed on disk.
	stored, ok, err := store.GetAuditoriumProfile("screen-3")
	if err != nil || !ok {
		t.Fatalf("profile not stored: ok=%v err=%v", ok, err)
	}
	if stored.LastQualification != "" {
		t.Errorf("persisted last_qualification = %q, want empty", stored.LastQualification)
	}
	// The settings ARE saved — §6.4 step 9 is what makes re-opening the room reuse
	// known-good settings; only the verdict is withheld.
	if stored.InputDeviceID != testScarlett || stored.ChannelMapping != "mono:HI" {
		t.Errorf("settings were not persisted: %+v", stored)
	}
	if stored.GainNotes != "attenuator pad inline" {
		t.Errorf("gain notes = %q, want the operator's §10.5 note", stored.GainNotes)
	}
}

// A real hardware measurement of the selected device is what §6.4 step 9 was
// written for, and it must still work.
func TestSavePersistsAVerdictMeasuredOnTheSelectedHardware(t *testing.T) {
	drv := &fakeCalDriver{
		list: calibration.DeviceList{
			HardwareCapture: true,
			Devices: []capture.Device{{
				ID: testScarlett, IDKind: capture.DeviceIDByID, Stable: true,
				FriendlyName: "Scarlett Solo — Analog", Channels: 2,
			}},
		},
		kind:       calibration.SourceHardware,
		streams:    []model.AudioStreamState{measuredStream(model.StreamHI, model.ChannelMono, testScarlett)},
		measuredMS: 9000,
	}
	s, store := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	_, body := postJSON(t, ts, "/api/admin/calibration/save", CalibrationSaveRequest{
		AuditoriumID: "screen-3", DeviceID: testScarlett,
		ChannelMapping: "mono:HI", Use: "HI", InputTier: "tier2_usb_interface",
	})
	var out CalibrationSaveResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if !out.Qualified {
		t.Fatalf("a settled hardware measurement should qualify the input: %s", out.Note)
	}
	stored, _, _ := store.GetAuditoriumProfile("screen-3")
	if stored.LastQualification != model.QualUsable {
		t.Errorf("persisted last_qualification = %q, want %q", stored.LastQualification, model.QualUsable)
	}
}

// A verdict read the instant the capture opened describes the start-up, not the
// booth's signal — the same trap §10.5's last_qualification already has.
func TestSaveWithholdsAVerdictThatHasNotSettled(t *testing.T) {
	drv := &fakeCalDriver{
		list: calibration.DeviceList{HardwareCapture: true, Devices: []capture.Device{{
			ID: testScarlett, IDKind: capture.DeviceIDByID, Stable: true, Channels: 2,
		}}},
		kind:       calibration.SourceHardware,
		streams:    []model.AudioStreamState{measuredStream(model.StreamHI, model.ChannelMono, testScarlett)},
		measuredMS: 40,
	}
	s, store := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	_, body := postJSON(t, ts, "/api/admin/calibration/save", CalibrationSaveRequest{
		AuditoriumID: "screen-3", DeviceID: testScarlett,
		ChannelMapping: "mono:HI", Use: "HI", InputTier: "tier2_usb_interface",
	})
	var out CalibrationSaveResponse
	_ = json.Unmarshal(body, &out)
	if out.Qualified {
		t.Fatal("a 40 ms reading must not be persisted as this input's qualification")
	}
	stored, _, _ := store.GetAuditoriumProfile("screen-3")
	if stored.LastQualification != "" {
		t.Errorf("persisted last_qualification = %q, want empty", stored.LastQualification)
	}
}

// §6.5: a mapping the detected hardware cannot satisfy is refused at the
// BOUNDARY, not merely hidden from the picker — the rule is "surfaced before a
// show", and a hand-built request must not route around it.
func TestSaveRefusesAMappingTheDeviceCannotSatisfy(t *testing.T) {
	drv := &fakeCalDriver{list: calibration.DeviceList{Devices: []capture.Device{{
		ID: "by-id:usb-CheapDongle-00,0", IDKind: capture.DeviceIDByID, Stable: true,
		FriendlyName: "Cheap Dongle", Channels: 1,
	}}}}
	s, store := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	resp, body := postJSON(t, ts, "/api/admin/calibration/save", CalibrationSaveRequest{
		AuditoriumID: "screen-3", DeviceID: "by-id:usb-CheapDongle-00,0",
		ChannelMapping: "stereo:L=HI,R=VI-N", Use: "dual", InputTier: "tier1_usb_dongle",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a stereo split on a mono device: %s", resp.StatusCode, body)
	}
	if _, ok, _ := store.GetAuditoriumProfile("screen-3"); ok {
		t.Error("an unsatisfiable mapping must not be written to the auditorium profile")
	}
}

// The picker must not let an installer choose a stereo split on a mono device in
// the first place (§6.5).
func TestPickerOffersNoStereoMappingOnAMonoDevice(t *testing.T) {
	drv := &fakeCalDriver{list: calibration.DeviceList{
		SyntheticDeviceID: "synthetic-tone", SyntheticLabel: "Synthetic test tone (no hardware)",
		HardwareDetail: "no ALSA capture source (BLOCKERS B3/B4)",
		Devices: []capture.Device{{
			ID: "by-id:usb-CheapDongle-00,0", IDKind: capture.DeviceIDByID, Stable: true,
			FriendlyName: "Cheap Dongle", Channels: 1,
		}},
	}}
	s, _ := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/admin/calibration/devices")
	if err != nil {
		t.Fatalf("GET devices: %v", err)
	}
	defer resp.Body.Close()
	var out CalibrationDevicesResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}

	var mono *CalibrationDevice
	for i := range out.Devices {
		if out.Devices[i].ID == "by-id:usb-CheapDongle-00,0" {
			mono = &out.Devices[i]
		}
	}
	if mono == nil {
		t.Fatalf("the detected device is missing from the picker: %+v", out.Devices)
	}
	for use, modes := range mono.Modes {
		for _, m := range modes {
			if strings.HasPrefix(m.Value, "stereo:") || strings.HasPrefix(m.Value, "summed:") ||
				strings.HasPrefix(m.Value, "stereo-pair:") {
				t.Errorf("use %q offers %q on a mono device (§6.5)", use, m.Value)
			}
		}
	}
	// And the picker must say plainly that nothing here can be measured.
	if mono.Measurable {
		t.Error("a device this build cannot open must not be marked measurable")
	}
	if mono.Detail == "" {
		t.Error("an unmeasurable device must carry the reason it cannot be measured")
	}
	// The stand-in is offered under its own name, never as a device's identity.
	if len(out.Devices) == 0 || !out.Devices[0].Synthetic {
		t.Errorf("the synthetic stand-in should be offered as itself: %+v", out.Devices)
	}
}

// A helper that is not answering is not "no devices attached" (guardrail #7).
func TestPickerSaysWhenTheMediaProcessIsNotAnswering(t *testing.T) {
	drv := &fakeCalDriver{fail: context.DeadlineExceeded}
	s, _ := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/admin/calibration/devices")
	if err != nil {
		t.Fatalf("GET devices: %v", err)
	}
	defer resp.Body.Close()
	var out CalibrationDevicesResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out.HelperError == "" {
		t.Fatal("an unreachable media process must be reported, not rendered as an empty device list")
	}
}

// §6.4 step 5: the live meter, and every frame of it carries its provenance.
func TestMeterStreamCarriesProvenanceOnEveryFrame(t *testing.T) {
	drv := &fakeCalDriver{
		list: calibration.DeviceList{SyntheticDeviceID: "synthetic-tone", SyntheticLabel: "Synthetic test tone"},
		kind: calibration.SourceSynthetic,
		streams: []model.AudioStreamState{
			measuredStream(model.StreamHI, model.ChannelMono, "synthetic-tone"),
		},
		measuredMS: 4000,
		detail:     "the synthetic tone source stood in for a booth feed",
	}
	s, _ := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+
		"/api/admin/calibration/meter?device_id=synthetic-tone&channel_mapping=mono%3AHI&use=HI&input_tier=tier2_usb_interface", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET meter: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q, want an SSE stream", got)
	}

	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	payload := strings.TrimPrefix(strings.TrimSpace(line), "data: ")
	var frame CalibrationReport
	if err := json.Unmarshal([]byte(payload), &frame); err != nil {
		t.Fatalf("decode frame %q: %v", payload, err)
	}
	if frame.SourceKind != string(calibration.SourceSynthetic) {
		t.Errorf("frame source kind = %q, want synthetic", frame.SourceKind)
	}
	if !frame.Measured || frame.Verdict == "" {
		t.Errorf("a frame with measurements must carry a verdict: %+v", frame)
	}
	if frame.Qualified {
		t.Error("a synthetic frame must never report itself as qualifying anything")
	}
	if frame.QualifiedDetail == "" {
		t.Error("a frame that cannot qualify must say why, in words a booth tech reads")
	}
	if len(frame.SummaryLines) == 0 {
		t.Error("§6.4 step 8's plain-language summary block is missing from the frame")
	}
}

// Nothing measured renders as NOT MEASURED — never a level, never a verdict.
func TestARefusedRunReportsNoVerdictAtAll(t *testing.T) {
	drv := &fakeCalDriver{
		list:    calibration.DeviceList{Devices: []capture.Device{{ID: testScarlett, Channels: 2}}},
		kind:    calibration.SourceNone,
		errText: "no capture backend for input device " + testScarlett + " in this build (BLOCKERS B3/B4)",
	}
	s, _ := newCalibrationServer(t, drv)
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	_, body := postJSON(t, ts, "/api/admin/calibration/measure", CalibrationSaveRequest{
		DeviceID: testScarlett, ChannelMapping: "mono:HI", Use: "HI", InputTier: "tier2_usb_interface",
	})
	var out CalibrationReport
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if out.Measured {
		t.Fatal("a refused run must not report itself as measured")
	}
	if out.Verdict != "" {
		t.Errorf("verdict = %q; a run that measured nothing has no verdict to show", out.Verdict)
	}
	if len(out.Streams) != 0 || out.Summary != nil {
		t.Errorf("a refused run must carry no levels and no summary: %+v", out)
	}
	if out.Error == "" {
		t.Error("the refusal must reach the screen (guardrail #7: fail loudly)")
	}
}

// The wizard is a DIAGNOSTIC surface. §8.4 keeps admin and patron separated
// structurally, not by authorization alone (§7.2 forbids any of this on a patron
// route: device names, dBFS figures, qualifier reasons).
func TestCalibrationRoutesAreNotReachableFromThePatronSurface(t *testing.T) {
	s, _ := newCalibrationServer(t, &fakeCalDriver{})
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	for _, path := range []string{
		"/admin/calibrate",
		"/api/admin/calibration/devices",
		"/api/admin/calibration/meter",
		"/api/admin/calibration/measure",
		"/api/admin/calibration/save",
		"/api/admin/calibration/stop",
	} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := bufio.NewReader(resp.Body).ReadString('\n')
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("patron surface answered %s with %d (%s); it must not exist there",
				path, resp.StatusCode, strings.TrimSpace(body))
		}
	}
}

// A page whose script queries elements the markup does not contain is a broken
// screen that every server-side test still passes. Pin the two together.
func TestCalibratePageContainsEveryElementItsScriptQueries(t *testing.T) {
	page, err := assetsFS.ReadFile("assets/calibrate.html")
	if err != nil {
		t.Fatalf("read page: %v", err)
	}
	script, err := assetsFS.ReadFile("assets/calibrate.js")
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	html := string(page)

	ids := map[string]bool{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`el\("([^"]+)"\)`),
		regexp.MustCompile(`getElementById\("([^"]+)"\)`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(script), -1) {
			ids[m[1]] = true
		}
	}
	if len(ids) < 10 {
		t.Fatalf("only found %d element ids in calibrate.js; the scan is not working", len(ids))
	}
	for id := range ids {
		if !strings.Contains(html, `id="`+id+`"`) {
			t.Errorf("calibrate.js uses #%s but calibrate.html has no such element", id)
		}
	}
	// The page must load its own script and stylesheet, or none of the above runs.
	for _, want := range []string{`src="/static/calibrate.js"`, `href="/static/calibrate.css"`} {
		if !strings.Contains(html, want) {
			t.Errorf("calibrate.html is missing %s", want)
		}
	}
}

// The page and its assets must actually be served on the admin listener.
func TestCalibratePageAndAssetsAreServed(t *testing.T) {
	s, _ := newCalibrationServer(t, &fakeCalDriver{})
	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()

	for _, tc := range []struct{ path, want string }{
		{"/admin/calibrate", "Audio Calibration Wizard"},
		{"/static/calibrate.js", "api/admin/calibration/meter"},
		{"/static/calibrate.css", ".provenance"},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s: %v", tc.path, err)
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s = %d", tc.path, resp.StatusCode)
			continue
		}
		if !strings.Contains(buf.String(), tc.want) {
			t.Errorf("GET %s did not contain %q", tc.path, tc.want)
		}
	}
}
