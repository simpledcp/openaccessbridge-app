package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
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
		"ice_scope",                   // §6.7/D27 which networks a patron was really offered
		"join_code",                   // §10.4 the patron who cannot scan
		"print_width_mm",              // §10.4 a QR printed too small does not scan
		"appliance_ready",             // §11.5 the appliance's own verdict, not the room's
		"staff_steps_outstanding",     // §11.5 the half the appliance cannot answer
		"endpoint_configured",         // §9.4 whether the appliance can be re-entered at all
		"tunnel_established",          // §9.4 a window is not a connection
		"ab_supported",                // §9.4 verified is not the same as installable
		"trusted_keys",                // §9.4 an appliance with none can verify nothing
		"password_set",                // §9.5 a passphrase is on file, not what it is
		"hex_ambiguity",               // §9.5 a value some scanners misread
		"ssh_introspected",            // §9.3 measured vs merely configured
		"unresolved_locators",         // §5.3 which locator forms have no handler
		"server_family",               // §5.3 and WHICH server emits them
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

// TestSyntheticAudioSourceIsNamedOnThePage: this codebase's first fabrication
// was a synthetic tone reported under a real device's identity. The media
// process refuses to do that now, and the readiness layer carries the marker —
// but a page that renders "HI · excellent · −13 dBFS" without the source makes
// the same claim about the auditorium's feed at the last step. Every place a
// booth tech reads an audio verdict has to carry the attribution.
func TestSyntheticAudioSourceIsNamedOnThePage(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	for _, want := range []string{
		"synthetic_source", // the server-derived marker
		"input_device_id",  // the identity the figures were measured from
		"SYNTHETIC SOURCE", // beside the quality verdict
		"BLOCKER B3",       // and why there is no real measurement
	} {
		if !strings.Contains(script, want) {
			t.Errorf("admin.js never renders %q — a generated tone reads as the booth feed", want)
		}
	}
	// The §10.6 verdict card is where a tech stops reading, so the caveat has to
	// reach it too, not only the detail tile.
	if !strings.Contains(script, "syntheticWhy") {
		t.Errorf("the audio verdict's reason line drops the synthetic attribution")
	}
	// And §10.8's persisted device profile is a bare string with no summary
	// beside it; recorded without a caveat it reads as qualified hardware.
	if !strings.Contains(script, "audioDeviceRow") {
		t.Errorf("the §10.8 audio device profile row does not distinguish a stand-in from hardware")
	}
}

// TestSyntheticPrefixIsTheOnePrefixTheMediaProcessUses: admin.js recognises the
// stand-in marker for §10.8's bare persisted string. If model's canonical prefix
// ever changes, that literal silently stops matching and the caveat disappears.
func TestSyntheticPrefixIsTheOnePrefixTheMediaProcessUses(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	if !strings.Contains(script, `"`+model.SyntheticDevicePrefix+`"`) {
		t.Errorf("admin.js does not use %q, so the §10.8 stand-in caveat cannot fire",
			model.SyntheticDevicePrefix)
	}
}

// TestZeroIsNotRenderedAsAMeasurement: the two payload fields that carry a
// plausible-looking number where they mean "nothing measured this" —
// drift_ms_estimate (an int64 that is 0 both for perfect sync and for a caption
// path with no timing engine) and level_dbfs (cleared to the silence floor
// whenever the media process reports a failure). Both are always present in the
// JSON, so a null check alone is dead code and the number always prints.
func TestZeroIsNotRenderedAsAMeasurement(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	if !strings.Contains(script, "caption_drift_measured") {
		t.Errorf("the drift row trusts the figure alone — a dead link then reads as flawless sync")
	}
	// The gate itself, not merely the helper's name: a helper that exists but is
	// never consulted leaves the sentinel on screen.
	if !strings.Contains(script, `sum.input_present === "no"`) {
		t.Errorf("the level row prints the payload figure unconditionally, sentinel and all")
	}
	if !strings.Contains(script, "cleared rather than measured") {
		t.Errorf("a cleared level must say it was cleared, not print the silence floor as a reading")
	}
}

// TestUnresolvedLocatorsAppearOnlyWhenObserved: the tally has no honest empty
// form. An always-present "Unresolved caption locators: none" row makes a claim
// on every render — that every locator resolved — which the appliance cannot
// support before it has fetched anything. Absent means no observation, and the
// row must be absent then.
func TestUnresolvedLocatorsAppearOnlyWhenObserved(t *testing.T) {
	script := mustAsset(t, "assets/admin.js")
	if !strings.Contains(script, "if (!list || !list.length) return null;") {
		t.Error("the unresolved-locator row does not suppress itself when nothing was observed")
	}
	// A null row only works if the renderer skips it; without the filter the
	// whole caption panel throws and every row above it disappears too.
	if !strings.Contains(script, "rows.filter(Boolean)") {
		t.Error("dl() does not skip null rows, so a conditional row breaks the panel")
	}
}
