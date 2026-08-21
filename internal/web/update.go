package web

// The §9.4 update surface: what an operator can see and do with a signed bundle
// on this appliance.
//
// The distinction this file exists to keep is between VERIFIED and INSTALLABLE.
// A bundle can be perfectly signed, current, and for this product, and still not
// be installable here — this image has no A/B slots (BLOCKERS B8). Letting "the
// bundle is good" read as "the update can be applied" would leave a booth
// waiting for something that is never going to happen, so both facts are carried
// separately all the way to the page.

import (
	"encoding/json"
	"net/http"

	"github.com/simpledcp/openaccessbridge/internal/update"
)

func (s *Server) handleAdminUpdate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		http.Error(w, "this appliance has no update service", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, s.deps.Update.Status())
}

func (s *Server) handleAdminUpdateVerify(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		http.Error(w, "this appliance has no update service", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
	}
	res, err := s.deps.Update.VerifyFile(body.Name)
	if err != nil {
		// A read or naming failure, as distinct from a bundle that was read and
		// found wanting — the latter is a 200 with OK:false, because the appliance
		// did its job and the answer is "no".
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, res)
}

// UpdateService is the seam internal/update fills. It is an interface so this
// package does not have to construct one, and so a test can hand in a stub.
type UpdateService interface {
	Status() update.Report
	VerifyFile(name string) (update.VerifyResult, error)
}
