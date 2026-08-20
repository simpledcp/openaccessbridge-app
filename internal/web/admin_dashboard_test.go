package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
)

// elementRefs finds every getElementById("…") the admin script performs.
var elementRefs = regexp.MustCompile(`el\("([a-z0-9-]+)"\)|getElementById\("([a-z0-9-]+)"\)`)

// TestAdminPageHasEveryElementItsScriptExpects: a dashboard whose JS silently
// throws on a missing node renders nothing at all, which on this appliance means
// a booth tech sees a blank page instead of "NOT READY". Tests passing is not
// the same as a working page, so this pins page and script together.
func TestAdminPageHasEveryElementItsScriptExpects(t *testing.T) {
	page := mustAsset(t, "assets/admin.html")
	script := mustAsset(t, "assets/admin.js")

	seen := map[string]bool{}
	for _, m := range elementRefs.FindAllStringSubmatch(script, -1) {
		id := m[1]
		if id == "" {
			id = m[2]
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("admin.js reads #%s but admin.html has no such element", id)
		}
	}
	// The script builds two verdict boxes by name; make sure the loop above
	// actually inspected something rather than silently matching nothing.
	if len(seen) < 8 {
		t.Fatalf("only %d element references found — the scan is not working", len(seen))
	}
	for _, id := range []string{"caption-big", "audio-big", "caption-verdict", "audio-verdict", "caption-why", "audio-why"} {
		if !strings.Contains(page, `id="`+id+`"`) {
			t.Errorf("the page is missing #%s, which setVerdict() addresses by name", id)
		}
	}
}

// TestDashboardRendersTheFactsTheAPIAlreadyMeasured: every fact the status
// endpoint has been returning and the page never showed. This is the whole
// reason this lane exists — the measurements were honest and invisible.
func TestDashboardRendersTheFactsTheAPIAlreadyMeasured(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	for _, field := range []string{
		"processes",                   // §3.5 per-process liveness
		"last_seen",                   // never-answered vs answered-a-while-ago
		"connected_caption_clients",   // §8.2 client count
		"max_concurrent_clients",      // against the D11 cap
		"firewall_introspected",       // measured vs merely asserted
		"connected_cinema_server",     // §8.5 who actually answered
		"allowed_cinema_server",       // §8.5 the pinned identity
		"identity_strength",           // §10.5 composite must be labelled
		"server_identity_check",       // §8.5 did the guard run, and pass
		"system_config",               // §10.8 what changed since this last worked
		"server_connected",            // §8.2 yes / no
		"timeline_state",              // §8.2 playing / paused / stopped
		"current_cue_visible",         // §8.2 visible / none
		"audio_streams",               // §8.2 present / active / clients / latency
		"latency_measured",            // §6.7 estimate is not a measurement
		"discovery_mode",              // §8.2
		"support_session",             // §8.2
		"deployment_banner",           // §8.4
		"admin_reachable_from_patron", // §8.4's raw condition, not the profile verdict
	} {
		if !strings.Contains(script, field) {
			t.Errorf("admin.js never reads %q — the fact is measured and invisible", field)
		}
	}
}

// TestUnverifiedFirewallDoesNotLookVerified: the sharpest honesty case on the
// page. /api/admin/status returns firewall_active AND firewall_introspected; a
// dashboard that prints "Active" for both is exactly the distinction the
// introspection work existed to create, thrown away at the last step.
func TestUnverifiedFirewallDoesNotLookVerified(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	if !strings.Contains(script, "firewall_introspected") {
		t.Fatalf("the page ignores whether the posture was measured")
	}
	if !strings.Contains(script, "UNVERIFIED") {
		t.Errorf("an asserted firewall must be labelled unverified, not rendered as active")
	}
	// And the two states must not share one rendering.
	if strings.Count(script, "active (verified)") != 1 || strings.Count(script, "active (UNVERIFIED)") != 1 {
		t.Errorf("verified and asserted postures must render differently")
	}
}

// TestEstimateIsNotRenderedAsAMeasurement: §8.2's latency row. The appliance
// holds only an encode-path estimate (§6.7 / D10 budgets are glass-to-ear,
// BLOCKER B3), so the page must say "not measured" and label the estimate.
func TestEstimateIsNotRenderedAsAMeasurement(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	if !strings.Contains(script, "latency_measured") {
		t.Fatalf("the latency row ignores whether anything was measured")
	}
	if !strings.Contains(script, "encode-path estimate only") {
		t.Errorf("the estimate must be labelled as an estimate")
	}
	if !strings.Contains(script, "not measured") {
		t.Errorf("an unmeasured latency must say so")
	}
}

// TestAdminPageCarriesNoCaptionText: the dashboard shows cue VISIBILITY only.
// Nothing on this page may reach for a cue's words (§9.4).
func TestAdminPageCarriesNoCaptionText(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	// ".text" as a property read — deliberately not matching ".textContent",
	// which is how this page writes into the DOM.
	cueTextRead := regexp.MustCompile(`\.text(?:$|[^A-Za-z])`)
	if m := cueTextRead.FindString(script); m != "" {
		t.Errorf("admin.js reads a cue text property (%q) — the admin surface renders visibility only", m)
	}
	for _, forbidden := range []string{"cue_text", "caption_text"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("admin.js reaches for %q — the admin surface renders cue visibility only", forbidden)
		}
	}
}

// TestAdminStatusNamesTheRoom: §8.5 wrong-room protection starts with knowing
// which auditorium the page in front of you describes.
func TestAdminStatusNamesTheRoom(t *testing.T) {
	s := newTestServer(t, model.ProfileLab, fanout.NewHub(fanout.Options{}))
	s.deps.Status = func() model.SystemReadiness {
		return model.SystemReadiness{CaptionReady: true, FirewallActive: true, FirewallIntrospected: true}
	}
	rec := httptest.NewRecorder()
	s.adminMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/admin/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["auditorium"] != "Auditorium 3" {
		t.Errorf("the payload must name the room, got %v", got["auditorium"])
	}
	// The readiness fields must still be inlined, not nested under a key.
	if _, ok := got["firewall_introspected"]; !ok {
		t.Errorf("the readiness fields must stay at the top level, got %v", got)
	}
}

// TestAdminPageIsServed: the route the booth actually opens.
func TestAdminPageIsServed(t *testing.T) {
	s := newTestServer(t, model.ProfileLab, fanout.NewHub(fanout.Options{}))
	rec := httptest.NewRecorder()
	s.adminMux().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /admin = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`id="deploy-banner"`, `id="warning-banner"`, "/static/admin.js", "/static/admin.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("the served page is missing %s", want)
		}
	}
}

func mustAsset(t *testing.T, name string) string {
	t.Helper()
	b, err := assetsFS.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}
