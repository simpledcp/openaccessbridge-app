package web

// §9.5's Wi-Fi onboarding code, on the operator surface.
//
// This is a SECOND code, printed beside the join URL's: one QR cannot be both a
// network join and a URL. A venue that wants one-scan onboarding prints the
// network code first and the page code second, and the appliance renders each.
//
// WHY THE PASSPHRASE IS STORED AT ALL (D35). A patron network's passphrase is
// handed to every person in the auditorium and printed on a lobby sign; it is
// not a secret in the security sense, and §9.5 asks plainly for a code that
// carries it. What it still is, is a credential the appliance did not previously
// hold — so it is never logged (§9.4), never served on the patron listener
// (§8.4), and the admin page says what it is rather than presenting it as a
// secret the appliance is protecting.

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/qr"
)

// WiFiResponse describes the configured patron network for the operator.
type WiFiResponse struct {
	// Configured: enough is known to render a joinable code.
	Configured bool   `json:"configured"`
	SSID       string `json:"ssid,omitempty"`
	Security   string `json:"security,omitempty"`
	Hidden     bool   `json:"hidden,omitempty"`
	// PasswordSet reports that a passphrase is on file WITHOUT returning it. The
	// QR carries it because a phone needs it; a status document polled by a
	// dashboard does not, and the two are different exposures.
	PasswordSet bool `json:"password_set"`
	// QRURL is the admin route serving the network code.
	QRURL string `json:"qr_url,omitempty"`
	// HexAmbiguity warns that an all-hexadecimal SSID or passphrase may be read
	// by some scanners as raw bytes rather than text. The appliance does not
	// guess a fix (both available ones break for somebody) — it tells the venue,
	// which can remove the ambiguity by changing one character.
	HexAmbiguity bool `json:"hex_ambiguity,omitempty"`
	// Note is the plain-language state for the page.
	Note string `json:"note,omitempty"`
}

// patronWiFi reads the configured network.
func (s *Server) patronWiFi() qr.WiFiNetwork {
	if s.deps.Store == nil {
		return qr.WiFiNetwork{}
	}
	ssid, _, _ := s.deps.Store.GetSetting(config.SettingPatronWiFiSSID)
	pw, _, _ := s.deps.Store.GetSetting(config.SettingPatronWiFiPassword)
	sec, _, _ := s.deps.Store.GetSetting(config.SettingPatronWiFiSecurity)
	hidden, _, _ := s.deps.Store.GetSetting(config.SettingPatronWiFiHidden)
	security := qr.WiFiSecurity(strings.TrimSpace(sec))
	switch security {
	case qr.WiFiWPA, qr.WiFiWEP, qr.WiFiOpen:
	default:
		// An unrecognised stored value is treated as WPA when there is a
		// passphrase and open when there is not, rather than rendering a code
		// with a security type no phone recognises.
		security = ""
	}
	return qr.WiFiNetwork{
		SSID:     strings.TrimSpace(ssid),
		Password: pw,
		Security: security,
		Hidden:   strings.TrimSpace(hidden) == "true",
	}
}

func (s *Server) handleAdminWiFi(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		http.Error(w, "no configuration store", http.StatusServiceUnavailable)
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, s.wifiResponse())
	case http.MethodPost:
		var body struct {
			SSID     string `json:"ssid"`
			Password string `json:"password"`
			Security string `json:"security"`
			Hidden   bool   `json:"hidden"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
		}
		ssid := strings.TrimSpace(body.SSID)
		security := qr.WiFiSecurity(strings.TrimSpace(body.Security))
		if ssid == "" {
			// Clearing: an operator removing the network removes the passphrase
			// with it. Leaving a stored passphrase behind for a network that is no
			// longer configured is exactly the stale credential nobody remembers.
			for _, k := range []string{
				config.SettingPatronWiFiSSID, config.SettingPatronWiFiPassword,
				config.SettingPatronWiFiSecurity, config.SettingPatronWiFiHidden,
			} {
				_ = s.deps.Store.PutSetting(k, "")
			}
			s.deps.Log.Info(logging.EvStateTransition, logging.Field("phase", "patron_wifi_cleared"))
			writeJSON(w, s.wifiResponse())
			return
		}
		switch security {
		case qr.WiFiWPA, qr.WiFiWEP, qr.WiFiOpen:
		case "":
			security = qr.WiFiWPA
			if strings.TrimSpace(body.Password) == "" {
				security = qr.WiFiOpen
			}
		default:
			writeJSONError(w, "security must be WPA, WEP or nopass", http.StatusBadRequest)
			return
		}
		if security != qr.WiFiOpen && strings.TrimSpace(body.Password) == "" {
			writeJSONError(w, "a secured network needs its passphrase, or the code will fail at the "+
				"moment a patron scans it", http.StatusBadRequest)
			return
		}
		_ = s.deps.Store.PutSetting(config.SettingPatronWiFiSSID, ssid)
		_ = s.deps.Store.PutSetting(config.SettingPatronWiFiPassword, body.Password)
		_ = s.deps.Store.PutSetting(config.SettingPatronWiFiSecurity, string(security))
		_ = s.deps.Store.PutSetting(config.SettingPatronWiFiHidden, strconv.FormatBool(body.Hidden))
		// §9.4: the SSID is operational configuration and travels; the passphrase
		// never does, not even hashed — there is nothing to compare it against
		// later, so a hash would be noise with a credential's shape.
		s.deps.Log.Info(logging.EvStateTransition,
			logging.Field("phase", "patron_wifi_configured"),
			logging.Field("ssid", ssid),
			logging.Field("security", string(security)))
		writeJSON(w, s.wifiResponse())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) wifiResponse() WiFiResponse {
	n := s.patronWiFi()
	out := WiFiResponse{
		SSID: n.SSID, Security: string(n.Security), Hidden: n.Hidden,
		PasswordSet:  strings.TrimSpace(n.Password) != "",
		Configured:   n.Configured(),
		HexAmbiguity: n.AllHexAmbiguity(),
	}
	switch {
	case !out.Configured && out.SSID == "":
		out.Note = "No patron network is configured, so there is no network code to print. " +
			"Patrons join the Wi-Fi from a sign and then scan the show's code."
	case !out.Configured:
		out.Note = "This network is marked as secured but has no passphrase on file, so a code " +
			"would fail at the moment a patron scans it."
	default:
		out.QRURL = "/api/admin/wifi-qr.png"
		out.Note = "Print this code beside the show's code: the network first, the page second. " +
			"One code cannot be both."
	}
	if out.HexAmbiguity {
		out.Note += " NOTE: this network's name or passphrase is entirely hexadecimal, and some " +
			"phone scanners read that as raw data rather than text. Changing one character to a " +
			"non-hex one removes the ambiguity."
	}
	return out
}

func (s *Server) handleAdminWiFiQR(w http.ResponseWriter, r *http.Request) {
	if s.deps.Store == nil {
		http.Error(w, "no configuration store", http.StatusServiceUnavailable)
		return
	}
	n := s.patronWiFi()
	if !n.Configured() {
		http.Error(w, "no patron network is configured", http.StatusNotFound)
		return
	}
	scale := qrDefaultScale
	if v := strings.TrimSpace(r.URL.Query().Get("scale")); v != "" {
		got, err := strconv.Atoi(v)
		if err != nil || got < 1 || got > qr.MaxScale {
			http.Error(w, "scale must be 1-"+strconv.Itoa(qr.MaxScale), http.StatusBadRequest)
			return
		}
		scale = got
	}
	sym, err := qr.Encode([]byte(qr.WiFiPayload(n)), qr.DefaultECC)
	if err != nil {
		http.Error(w, "this network cannot be encoded as a QR", http.StatusInternalServerError)
		return
	}
	writePNG(w, sym, scale)
}
