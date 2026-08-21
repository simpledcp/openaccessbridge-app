package web

// The §9.4 support session on the operator surface.
//
// This is the only page in the product that opens a way INTO the appliance, so
// it is written to make the state of that way in unmistakable. Three things it
// deliberately does:
//
//   - It refuses to open a session without a reason. §9.4 asks for an audited
//     feature, and an audit entry with no reason is a timestamp.
//   - It never reports a window as a connection. This appliance permits an
//     outbound destination; it has no tunnel client and dials nothing, and the
//     response carries that distinction as a field rather than as prose.
//   - It shows the RECENT TRAIL beside the switch. A support mode whose history
//     is only visible in a database is one nobody reviews.

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/support"
)

// SupportResponse is the operator-facing view.
type SupportResponse struct {
	support.Report
	// Recent is the audit trail, newest first, so the switch and its history are
	// on the same screen.
	Recent []model.SupportSession `json:"recent,omitempty"`
	// MaxMinutes is the ceiling the appliance will grant whatever is asked for.
	MaxMinutes int `json:"max_minutes"`
}

func (s *Server) handleAdminSupport(w http.ResponseWriter, r *http.Request) {
	if s.deps.Support == nil {
		http.Error(w, "support sessions are not available on this appliance", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.supportResponse())
	case http.MethodPost:
		var body struct {
			Reason  string `json:"reason"`
			Minutes int    `json:"minutes"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		}
		// The authenticated operator, for the audit trail. Only a hash of it is
		// stored, and the name never reaches a log (§9.4).
		user, _, _ := r.BasicAuth()
		_, err := s.deps.Support.Open(support.OpenRequest{
			Reason:    body.Reason,
			AdminUser: user,
			Duration:  time.Duration(body.Minutes) * time.Minute,
		})
		if err != nil {
			// The reason is operator-facing here — this is the admin surface, and
			// "no endpoint is configured" is exactly what a booth needs to read.
			// It is returned as JSON so the page never has to read a response as
			// text, which is a habit the admin surface deliberately does not have
			// (the dashboard must not learn to render bodies it did not parse).
			writeJSONError(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, s.supportResponse())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminSupportEndpoint configures WHERE a support session may reach.
//
// This exists because the session manager was built with no way for an operator
// to give it an endpoint: every setting had a reader and no writer, so support
// mode could not be opened on any appliance a person had actually set up. A
// capability with no production caller protects nobody, and one an operator
// cannot reach is the same defect wearing a different hat.
//
// The host is validated as a LITERAL IP here as well as in the firewall's rule
// generation. Two checks rather than one because they answer different
// questions: this one tells an operator their entry is wrong while they are
// typing it, and the firewall's refuses to emit a rule it cannot scope. Neither
// is redundant — an appliance whose only validation happens at rule-generation
// time reports "support mode is configured" for a value that will never work.
func (s *Server) handleAdminSupportEndpoint(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		http.Error(w, "no configuration store", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Host  string `json:"host"`
		Port  int    `json:"port"`
		Iface string `json:"iface"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
	}
	host := strings.TrimSpace(body.Host)
	iface := strings.TrimSpace(body.Iface)

	// An empty submission CLEARS the endpoint, which is how an operator turns off
	// the ability to open support mode at all. It is an explicit action with an
	// explicit result, not a validation failure.
	if host == "" && iface == "" && body.Port == 0 {
		for _, k := range []string{
			config.SettingSupportEndpointHost,
			config.SettingSupportEndpointPort,
			config.SettingSupportEgressIface,
		} {
			_ = s.deps.Store.PutSetting(k, "")
		}
		s.deps.Log.Warn(logging.EvSupportSessionClosed,
			logging.Field("phase", "support_endpoint_cleared"))
		writeJSON(w, s.supportResponse())
		return
	}

	if net.ParseIP(host) == nil {
		writeJSONError(w, "the support endpoint must be a literal IP address, not a name: "+
			"a name needs DNS from an appliance whose §9.1 checks exist to catch DNS leaks, "+
			"and a name that resolves differently tomorrow is a rule that quietly changes what it permits",
			http.StatusBadRequest)
		return
	}
	if body.Port < 1 || body.Port > 65535 {
		writeJSONError(w, "the support endpoint port must be 1-65535", http.StatusBadRequest)
		return
	}
	if iface == "" {
		writeJSONError(w, "an egress interface is required: on this appliance both the theater side "+
			"and the patron side are possible, and guessing would put support traffic on the patron LAN",
			http.StatusBadRequest)
		return
	}
	if err := s.deps.Store.PutSetting(config.SettingSupportEndpointHost, host); err != nil {
		writeJSONError(w, "could not save the support endpoint", http.StatusInternalServerError)
		return
	}
	_ = s.deps.Store.PutSetting(config.SettingSupportEndpointPort, strconv.Itoa(body.Port))
	_ = s.deps.Store.PutSetting(config.SettingSupportEgressIface, iface)
	// Audited: changing where the appliance may be reached from is exactly as
	// significant as opening a window to it. The address is operational
	// configuration, not a client identifier, so it travels.
	s.deps.Log.Warn(logging.EvSupportSessionOpened,
		logging.Field("phase", "support_endpoint_configured"),
		logging.Field("endpoint", iface+":"+host+":"+strconv.Itoa(body.Port)))
	writeJSON(w, s.supportResponse())
}

func (s *Server) handleAdminSupportClose(w http.ResponseWriter, r *http.Request) {
	if s.deps.Support == nil {
		http.Error(w, "support sessions are not available on this appliance", http.StatusServiceUnavailable)
		return
	}
	if err := s.deps.Support.Close(); err != nil {
		writeJSONError(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, s.supportResponse())
}

func (s *Server) supportResponse() SupportResponse {
	out := SupportResponse{
		Report:     s.deps.Support.Status(),
		MaxMinutes: int(model.MaxSupportSession / time.Minute),
	}
	if s.deps.SupportTrail != nil {
		if recent, err := s.deps.SupportTrail(10); err == nil {
			out.Recent = recent
		}
	}
	return out
}

// writeJSONError returns an operator-facing reason as JSON.
func writeJSONError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
