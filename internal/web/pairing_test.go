package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

func saveCalibration(t *testing.T, s *Server, body string) CalibrationSaveResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	s.AdminHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/api/admin/calibration/save", strings.NewReader(body)))
	var out CalibrationSaveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("save (%d): %s", rec.Code, rec.Body.String())
	}
	return out
}

// §6.5's two-device room is built one device at a time, because the wizard
// measures one input at a time. Before the profile could hold a per-stream
// device, the second save overwrote the first and the pairing was lost.
func TestCalibratingTwoDevicesBuildsOnePairing(t *testing.T) {
	s, _ := newCalibrationServer(t, &fakeCalDriver{})

	first := saveCalibration(t, s, `{"auditorium_id":"screen-3","device_id":"synthetic-hi-dongle",
		"channel_mapping":"mono:HI","use":"HI","part_of_pairing":true}`)
	if !first.Saved {
		t.Fatalf("first half was not saved: %s", first.Error)
	}
	// It must SAY the room is unfinished: silence here is how the second device
	// gets forgotten.
	if !strings.Contains(first.Note, "NOT finished") || !strings.Contains(first.Note, "VI-N") {
		t.Fatalf("the note does not say VI-N is still unassigned: %q", first.Note)
	}

	second := saveCalibration(t, s, `{"auditorium_id":"screen-3","device_id":"synthetic-vin-dongle",
		"channel_mapping":"mono:VI-N","use":"VI-N","part_of_pairing":true}`)
	if !second.Saved {
		t.Fatalf("second half was not saved: %s", second.Error)
	}
	if strings.Contains(second.Note, "NOT finished") {
		t.Fatalf("the completed pairing still reads as unfinished: %q", second.Note)
	}

	prof := second.Profile
	if prof.ChannelMapping != "multi-device" {
		t.Fatalf("the saved mapping is %q", prof.ChannelMapping)
	}
	if got := prof.StreamDevices[model.StreamHI]; got != "synthetic-hi-dongle" {
		t.Errorf("HI is assigned %q — the first device was overwritten", got)
	}
	if got := prof.StreamDevices[model.StreamVIN]; got != "synthetic-vin-dongle" {
		t.Errorf("VI-N is assigned %q", got)
	}
	if err := prof.ValidateInputs(true, []model.StreamType{model.StreamHI, model.StreamVIN}); err != nil {
		t.Fatalf("the saved pairing is not coherent: %v", err)
	}
}

// A pairing half must be a SINGLE-stream mapping. A stereo split would measure
// two streams from one device and then claim to be half of a two-device room —
// a configuration that describes neither thing it might mean.
func TestAPairingHalfMustFeedExactlyOneStream(t *testing.T) {
	s, _ := newCalibrationServer(t, &fakeCalDriver{})
	out := saveCalibration(t, s, `{"auditorium_id":"screen-3","device_id":"synthetic-dongle",
		"channel_mapping":"stereo:L=HI,R=VI-N","use":"dual","part_of_pairing":true}`)
	if out.Saved {
		t.Fatal("a stereo split was accepted as one half of a pairing")
	}
	if !strings.Contains(out.Error, "one stream at a time") {
		t.Fatalf("the refusal does not explain how to calibrate a pairing: %q", out.Error)
	}
}

// Going back to a single-device room must clear the pairing. A stale per-stream
// device left behind would mean the room ran on a mapping nobody chose.
func TestASingleDeviceSaveClearsAStalePairing(t *testing.T) {
	s, _ := newCalibrationServer(t, &fakeCalDriver{})
	saveCalibration(t, s, `{"auditorium_id":"screen-3","device_id":"synthetic-hi-dongle",
		"channel_mapping":"mono:HI","use":"HI","part_of_pairing":true}`)

	out := saveCalibration(t, s, `{"auditorium_id":"screen-3","device_id":"synthetic-one-dongle",
		"channel_mapping":"stereo:L=HI,R=VI-N","use":"dual"}`)
	if !out.Saved {
		t.Fatalf("the single-device save failed: %s", out.Error)
	}
	if len(out.Profile.StreamDevices) != 0 {
		t.Fatalf("a stale pairing survived a single-device save: %v", out.Profile.StreamDevices)
	}
	if out.Profile.ChannelMapping != "stereo:L=HI,R=VI-N" {
		t.Fatalf("the mapping is %q", out.Profile.ChannelMapping)
	}
}
