package readiness

// The §11.5 pre-show QA checklist, derived from what the appliance measured.
//
// §11.5: "The install/admin readiness checks (Sections 8.1, 9.3) double as the
// field QA gate. Codify a staff pre-show checklist with explicit pass/fail …
// Crucially, define what staff do when a check fails before a show — readiness
// checks are only useful if they're tied to a procedure."
//
// So this file is not a second measurement layer. Every measured item reads a
// fact the dashboard already reports; what it adds is the two things a dashboard
// does not have: an explicit pass/fail per item and, for each one, what a person
// does about it in the ninety seconds before a house opens. A checklist that
// tells a booth "audio_ready: false" and stops has moved the problem, not solved
// it.
//
// It is derived from model.SystemReadiness rather than from readiness.Inputs on
// purpose. The dashboard verdict is the appliance's account of itself, and the
// checklist must be a view OF that same account — not a parallel derivation that
// could disagree with the page the operator is looking at.

import (
	"fmt"
	"strings"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// ChecklistFacts are the few things the checklist needs that the dashboard
// verdict does not carry, supplied by the surface that owns them.
type ChecklistFacts struct {
	// PhysicalPortLabel is §8.5's re-cabling defence: "Tie the auditorium profile
	// to the physical port label so a re-cabling that swaps rooms is caught at
	// setup, not mid-show." The appliance cannot see a cable, so the label it was
	// given is printed and a person confirms it.
	PhysicalPortLabel string
	// Auditorium is the room's own name (§8.5), which the dashboard verdict does
	// not carry — SystemReadiness names the cinema SERVER, which is a different
	// thing and mixing them up is precisely the wrong-room confusion §8.5 is
	// about.
	Auditorium string
	// SessionIssued: a per-show patron session exists (§10.4). The session store
	// lives in internal/web, so this crosses in rather than being re-derived.
	SessionIssued bool
	// SessionExpiresAt is that session's expiry, zero when none is live.
	SessionExpiresAt time.Time
	// JoinCodeIssued: patrons have a short code to type as well as a QR.
	JoinCodeIssued bool
	// PerShowTokenRequired mirrors the profile: when false, the patron page is
	// open without a code and a missing session is not a fault.
	PerShowTokenRequired bool
	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// BuildChecklist derives the §11.5 sheet.
func BuildChecklist(st model.SystemReadiness, facts ChecklistFacts) model.PreShowChecklist {
	now := facts.Now
	if now == nil {
		now = time.Now
	}
	checks := []model.PreShowCheck{}
	checks = append(checks, securityChecks(st)...)
	checks = append(checks, captionChecks(st)...)
	checks = append(checks, audioChecks(st, DefaultLatencyBudgets())...)
	checks = append(checks, patronChecks(st, facts, now())...)
	checks = append(checks, staffChecks(st, facts.PhysicalPortLabel)...)

	out := model.PreShowChecklist{
		Auditorium:  facts.Auditorium,
		GeneratedAt: now().UTC().Format(time.RFC3339),
		Checks:      checks,
	}
	// ApplianceReady counts only what the appliance can answer. A blocking item
	// that was never run counts against it exactly as a failure does: the whole
	// point of the three states is that "we did not look" must not buy the same
	// silence as "we looked and it was fine".
	out.ApplianceReady = true
	for _, c := range checks {
		if c.Source == model.SourceStaff {
			out.StaffStepsOutstanding++
			continue
		}
		if c.Blocking && !c.Passing() {
			out.ApplianceReady = false
			out.BlockingProblems = append(out.BlockingProblems, c.ID)
		}
	}
	return out
}

func securityChecks(st model.SystemReadiness) []model.PreShowCheck {
	firewall := model.PreShowCheck{
		ID: "firewall_active", Title: "Firewall active and verified", Section: "§9.1, §9.3",
		Source: model.SourceMeasured, Blocking: true,
		Remedy: "Do not open the house on an unprotected appliance. Check that openaccessbridge-firewall " +
			"is running as root (systemctl status openaccessbridge-firewall); if it is running and the " +
			"ruleset still cannot be read, escalate — the posture is unknown, not safe.",
	}
	switch {
	case st.FirewallActive && st.FirewallIntrospected:
		firewall.State, firewall.Detail = model.CheckPass, "the loaded ruleset was read back and matches this profile"
	case !st.FirewallIntrospected:
		// D17, fail-loud never fail-wrong: an unread ruleset is not a failed one,
		// and it is certainly not a passing one.
		firewall.State = model.CheckNotRun
		firewall.Detail = "nothing read the loaded ruleset, so the posture is unverified rather than confirmed"
	default:
		firewall.State, firewall.Detail = model.CheckFail, "the expected ruleset is not loaded"
	}

	forwarding := model.PreShowCheck{
		ID: "forwarding_disabled", Title: "IP forwarding disabled", Section: "§9.1",
		Source: model.SourceMeasured, Blocking: true,
		Remedy: "The patron network can reach the theater network. Stop the show setup and escalate: " +
			"this is the guardrail the whole deployment rests on.",
	}
	forwarding.State = passFail(st.ForwardingDisabled)
	if st.ForwardingDisabled {
		forwarding.Detail = "v4 and v6 forwarding are off"
	} else {
		forwarding.Detail = "forwarding is ENABLED — patron traffic could route to the theater side"
	}

	admin := model.PreShowCheck{
		ID: "admin_not_on_patron_net", Title: "Admin surface not reachable from the patron network",
		Section: "§8.4", Source: model.SourceMeasured, Blocking: st.DeploymentProfile.Production(),
		Remedy: "Re-bind the admin surface off the patron interface (--admin-bind 127.0.0.1:8090) and " +
			"restart core. In Lab and Booth Diagnostic this is permitted behind the banner, but never " +
			"with an audience in the room.",
	}
	switch st.AdminOnPatronNet {
	case model.TriNo:
		admin.State, admin.Detail = model.CheckPass, "the admin bind is not reachable from the patron side"
	case model.TriYes:
		admin.State, admin.Detail = model.CheckFail, "a phone on the patron SSID can reach /admin"
	default:
		admin.State, admin.Detail = model.CheckNotRun, "nothing measured the admin surface's reachability"
	}

	creds := model.PreShowCheck{
		ID: "no_default_credentials", Title: "No default admin credentials", Section: "§8.1",
		Source: model.SourceMeasured, Blocking: true,
		State:  passFail(!st.DefaultCreds),
		Remedy: "Set a real admin password in the setup wizard before the house opens.",
	}
	if st.DefaultCreds {
		creds.Detail = "a default credential is still in place"
	} else {
		creds.Detail = "no default credential"
	}

	ssh := model.PreShowCheck{
		ID: "ssh_closed", Title: "SSH is not a way into this appliance", Section: "§9.1, §9.3",
		Source: model.SourceMeasured, Blocking: st.DeploymentProfile.Production(),
		Remedy: "Disable sshd on this appliance (§9.1 makes support mode the only re-entry path). " +
			"If it must stay on for an install, close it before the house opens.",
	}
	switch {
	case !st.SSHIntrospected:
		// D17/D37: an unmeasured way in is neither open nor closed, and this row
		// used to claim closed on exactly this evidence.
		ssh.State = model.CheckNotRun
		ssh.Detail = "nothing could measure whether SSH is listening, so it is unknown whether this " +
			"appliance can still be reached over SSH"
	case st.SSHNetworkReachable:
		ssh.State = model.CheckFail
		ssh.Detail = "sshd is accepting connections from the network"
	default:
		ssh.State = model.CheckPass
		ssh.Detail = "nothing is listening for SSH on a network"
	}

	return []model.PreShowCheck{firewall, forwarding, admin, creds, ssh}
}

func captionChecks(st model.SystemReadiness) []model.PreShowCheck {
	link := st.CaptionLink

	connected := model.PreShowCheck{
		ID: "server_connected", Title: "Cinema server connected, lease active", Section: "§5.2, §8.2",
		Source: model.SourceMeasured, Blocking: true,
		Remedy: "Check the theater-side cable and that the server's ACSP endpoint is configured for this " +
			"auditorium. Captions cannot start without a lease, and this is the single most common " +
			"pre-show fault.",
	}
	connected.State = passFail(link.LeaseActive)
	if link.LeaseActive {
		connected.Detail = fmt.Sprintf("lease active, renews every %d s", link.RequestIntervalMS/1000)
	} else {
		connected.Detail = "no active lease with the cinema server"
	}

	room := model.PreShowCheck{
		ID: "right_auditorium", Title: "Connected to THIS auditorium's server", Section: "§8.5",
		Source: model.SourceMeasured, Blocking: true,
		Remedy: "Stop. A mismatch means this appliance is serving another room's captions — the failure " +
			"a multiplex actually produces. Check the pinned server for this auditorium in the setup " +
			"wizard against the server that answered.",
	}
	switch st.IdentityCheck {
	case "match":
		room.State, room.Detail = model.CheckPass, "the connected server matches the identity pinned for this room"
	case "mismatch":
		room.State, room.Detail = model.CheckFail, "the connected server is NOT the one pinned for this room"
	case "unpinned":
		room.State = model.CheckNotRun
		room.Detail = "no server identity is pinned for this auditorium, so nothing can be compared"
	default:
		room.State = model.CheckNotRun
		room.Detail = "the wrong-room guard has not reported"
	}

	assets := model.PreShowCheck{
		ID: "caption_assets_loaded", Title: "RPL and caption assets loaded", Section: "§5.3",
		Source: model.SourceMeasured, Blocking: true,
		Remedy: "Ask the projectionist to load the show's playlist. If the RPL is loaded and the assets are " +
			"not, the timed-text track file is missing or unreachable — that is a content problem in the " +
			"DCP, not an appliance problem, and the show will play with blank captions.",
	}
	switch {
	case link.RPLLoaded && link.AssetsLoaded:
		assets.State, assets.Detail = model.CheckPass, "playlist and timed-text assets are loaded"
	case link.RPLLoaded:
		assets.State, assets.Detail = model.CheckFail, "the playlist is loaded but its caption assets are not"
	default:
		assets.State, assets.Detail = model.CheckFail, "no playlist has been received"
	}

	lang := model.PreShowCheck{
		ID: "caption_language_available", Title: "The advertised caption language is available",
		Section: "§5.3, §7.1", Source: model.SourceMeasured, Blocking: true,
		Remedy: "Check the show's advertised accessibility language against what the DCP carries. If the " +
			"language a patron was sold is not in the list, tell the front of house BEFORE the house " +
			"opens — this is an access-equivalence problem, not a technical one.",
	}
	if len(link.AvailableLanguages) > 0 {
		lang.State = model.CheckPass
		lang.Detail = "available: " + strings.Join(link.AvailableLanguages, ", ")
	} else {
		lang.State, lang.Detail = model.CheckFail, "no caption language is available"
	}

	return []model.PreShowCheck{connected, room, assets, lang}
}

func audioChecks(st model.SystemReadiness, budgets LatencyBudgets) []model.PreShowCheck {
	var out []model.PreShowCheck
	wanted := []struct {
		stream model.StreamType
		title  string
	}{
		{model.StreamHI, "HI feed present and qualified"},
		{model.StreamVIN, "VI-N feed present and qualified"},
	}
	for _, w := range wanted {
		c := model.PreShowCheck{
			ID:    "audio_" + strings.ToLower(strings.ReplaceAll(string(w.stream), "-", "_")),
			Title: w.title, Section: "§6.3, §6.6", Source: model.SourceMeasured, Blocking: true,
			Remedy: "Check the feed's cable at the booth end and re-run the §6.4 calibration wizard for this " +
				"input. If the input is silent while the film is playing, the fault is upstream of this " +
				"appliance — the processor's output, not the capture.",
		}
		var found *model.AudioStreamSummary
		for i := range st.AudioStreams {
			if st.AudioStreams[i].StreamType == w.stream {
				found = &st.AudioStreams[i]
				break
			}
		}
		switch {
		case found == nil:
			c.State, c.Detail = model.CheckNotRun, "this feed is not configured on this appliance"
		case found.SyntheticSource:
			// The sharpest honesty line on the sheet. A stand-in tone qualifying
			// perfectly is not this auditorium's audio, and a green tick here
			// would send a booth to open a house on a test tone.
			c.State = model.CheckUnmeasurable
			c.Detail = "this feed is a generated TEST TONE, not this auditorium's audio — no capture hardware (BLOCKER B3)"
			c.Remedy = "Do not present this as the room's audio. Real capture hardware has to be connected " +
				"and the §6.4 wizard re-run against it before this item can mean anything."
		case found.InputPresent == model.TriYes && found.StreamActive == model.TriYes &&
			(found.Quality == model.QualExcellent || found.Quality == model.QualUsable):
			c.State = model.CheckPass
			c.Detail = fmt.Sprintf("present, streaming, quality %s", found.Quality)
		case found.InputPresent != model.TriYes:
			c.State, c.Detail = model.CheckFail, "no signal on this input"
		case found.StreamActive != model.TriYes:
			c.State, c.Detail = model.CheckFail, "the input is present but nothing is being delivered to patrons"
		default:
			c.State = model.CheckFail
			c.Detail = fmt.Sprintf("quality %s", found.Quality)
		}
		out = append(out, c)
	}

	// §11.5 lists "latency within threshold". It is not measurable here and
	// saying so is the entire value of the line: a booth that sees a tick beside
	// a latency it believes was verified has been told something nobody knows.
	lat := model.PreShowCheck{
		ID: "audio_latency_within_budget", Title: "Audio latency within the §6.7 budget",
		Section: "§6.7, §11.4", Source: model.SourceMeasured, Blocking: false,
		State: model.CheckUnmeasurable,
		Detail: "no glass-to-ear latency has ever been measured on this appliance. The appliance-side " +
			"floor is measured (~10.7 ms), but the dominant term is the phone's own jitter buffer and " +
			"nothing here can see it (BLOCKERS B2, B3)",
		Remedy: "Treat the HI budget as unverified for this venue. If patrons report the audio lagging the " +
			"picture, that is a real report and not a misconfiguration — log it and escalate; it is the " +
			"measurement §11.4 exists to take.",
	}
	// If a real end-to-end figure ever exists, judge it against the BUDGET this
	// item's title names. The first version passed anything above zero, so a
	// measured 4-second latency would have printed PASS beside the words "within
	// the §6.7 budget" — a check that reads as the opposite of what it did.
	for _, s := range st.AudioStreams {
		if !s.LatencyMeasured {
			continue
		}
		budget := budgets.VINMaxMS
		if s.StreamType == model.StreamHI {
			budget = budgets.HIMaxMS
		}
		lat.State = passFail(s.LatencyMS > 0 && s.LatencyMS <= budget)
		lat.Detail = fmt.Sprintf("%s measured at %d ms against a %d ms budget", s.StreamType, s.LatencyMS, budget)
		if lat.State == model.CheckFail {
			lat.Blocking = false // a real measurement over budget is a venue decision, not a refusal to open
			break
		}
	}
	out = append(out, lat)
	return out
}

func patronChecks(st model.SystemReadiness, facts ChecklistFacts, now time.Time) []model.PreShowCheck {
	session := model.PreShowCheck{
		ID: "show_session_issued", Title: "A per-show join code has been issued", Section: "§10.4, §8.5",
		Source: model.SourceMeasured, Blocking: facts.PerShowTokenRequired,
		Remedy: "Issue tonight's session from the admin dashboard and post the QR and code where patrons " +
			"can see them. Never re-use yesterday's card: a stale code is how patrons end up in the " +
			"wrong room.",
	}
	switch {
	case !facts.PerShowTokenRequired:
		session.State = model.CheckPass
		session.Detail = "this deployment profile does not require a per-show token; the patron page is open"
	case !facts.SessionIssued:
		session.State, session.Detail = model.CheckFail, "no session is live, so no patron can join"
	case !facts.SessionExpiresAt.IsZero() && !facts.SessionExpiresAt.After(now):
		session.State, session.Detail = model.CheckFail, "the live session has already expired"
	default:
		session.State = model.CheckPass
		session.Detail = "a session is live"
		if facts.JoinCodeIssued {
			session.Detail += " with a typed code as well as a QR"
		}
	}

	scope := model.PreShowCheck{
		ID: "patron_network_scoped", Title: "Patron audio is offered on the patron network only",
		Section: "§9.1, §6.7", Source: model.SourceMeasured, Blocking: false,
		Remedy: "Set the patron interface in the setup wizard. Phones will otherwise spend their join time " +
			"trying to reach this appliance on networks they cannot route to, which is slowest exactly " +
			"when the whole house joins at once.",
	}
	// Read from the MEASUREMENT, not from the intent flag. This item is labelled
	// SourceMeasured, and an earlier version passed on InForce alone — which
	// meant it printed PASS for an appliance that was confined on paper and
	// offering something else in fact, and for one whose last patron was offered
	// nothing at all.
	sc := st.ICEScope
	switch {
	case sc.Unusable != "":
		scope.State, scope.Detail = model.CheckFail, "the configured patron interface cannot be used: "+sc.Unusable
		scope.Blocking = true
	case !sc.Answered:
		scope.State = model.CheckNotRun
		if sc.InForce {
			scope.Detail = "gathering is set to " + sc.Interface +
				", but no patron has joined yet, so nothing has been offered to measure"
		} else {
			scope.Detail = "no patron interface is configured, and no patron has joined yet"
		}
	case sc.OfferedCandidates == 0:
		scope.State = model.CheckFail
		scope.Detail = "the last patron was offered no way to connect at all"
		scope.Blocking = true
	case sc.InForce && len(sc.OfferedInterfaces) == 1 && sc.OfferedInterfaces[0] == sc.Interface:
		scope.State = model.CheckPass
		scope.Detail = "the last patron was offered " + sc.Interface + " only"
	case sc.InForce:
		scope.State = model.CheckFail
		scope.Detail = "configured for " + sc.Interface + " but the last patron was offered " +
			strings.Join(sc.OfferedInterfaces, ", ")
		scope.Blocking = true
	default:
		scope.State = model.CheckNotRun
		scope.Detail = "no patron interface is configured; the last patron was offered " +
			strings.Join(sc.OfferedInterfaces, ", ")
	}

	room := model.PreShowCheck{
		ID: "auditorium_named", Title: "This appliance knows which room it is",
		Section: "§7.2, §8.5", Source: model.SourceMeasured, Blocking: true,
		Remedy: "Name this auditorium in the setup wizard. Until then the patron page shows no room " +
			"name, and the last human check against a wrong-room join — a patron or passing staff " +
			"member reading the room off the page — cannot happen.",
	}
	if strings.TrimSpace(facts.Auditorium) == "" {
		room.State = model.CheckFail
		room.Detail = "this appliance has not been told which auditorium it serves"
	} else {
		room.State = model.CheckPass
		room.Detail = "the patron page shows " + facts.Auditorium
	}

	return []model.PreShowCheck{session, scope, room}
}

// staffChecks are the items only a person can answer. The appliance prints the
// step and the pass criterion; it records NOTHING about whether it was done.
//
// This is where a checklist normally acquires its lie. A tick box that the
// appliance stores and shows back looks exactly like a measurement a week later,
// and this repo has already shipped that once (a §10.8 readiness stamp written
// by pressing a button). So these items are always outstanding, every time the
// sheet is generated, and the sheet says so.
func staffChecks(st model.SystemReadiness, portLabel string) []model.PreShowCheck {
	patronNet := model.PreShowCheck{
		ID: "patron_page_reachable_from_seats", Title: "Patron page loads on the patron Wi-Fi, FROM THE SEATS",
		Section: "§11.5, §9.5", Source: model.SourceStaff, Blocking: true,
		Detail: "Join the patron SSID on a phone, walk to a seat far from the booth, and load the patron " +
			"page. It must load and audio must start there — not at the rack.",
		Remedy: "If it works at the rack and not in the seats, the fault is AP placement or RF, not the " +
			"appliance. §9.5 names this: a booth-mounted AP can pass bench testing and fail in the room.",
	}
	adminHidden := model.PreShowCheck{
		ID: "admin_hidden_from_patron_ssid", Title: "Admin dashboard is not reachable from the patron SSID",
		Section: "§8.4, §11.5", Source: model.SourceStaff, Blocking: true,
		Detail: "On the same phone, on the patron SSID, try the admin address. It must not load.",
		Remedy: "If it loads, take the appliance out of service for this show and re-bind the admin " +
			"surface. The appliance's own check answers for its BINDING; only this one answers for " +
			"what the network actually permits.",
	}
	cable := model.PreShowCheck{
		ID: "theater_cable_in_the_labelled_port", Title: "The theater-side cable is in the labelled port",
		Section: "§8.5", Source: model.SourceStaff, Blocking: false,
		Remedy: "If the cable is in a different port, this appliance is very likely serving another " +
			"room. Stop and check the rack against the auditorium profile before the house opens — " +
			"this is the re-cabling failure §8.5 exists to catch, and it is invisible from software.",
	}
	if label := strings.TrimSpace(portLabel); label != "" {
		cable.Detail = "This auditorium's profile says the theater side is on: " + label +
			". Confirm the cable is physically there."
	} else {
		cable.Detail = "No physical port label is recorded for this auditorium, so there is nothing " +
			"to check the rack against. Record one in the setup wizard."
		cable.Remedy = "Record the port label in the setup wizard. Without it a re-cabling that swaps " +
			"two rooms cannot be caught at setup, which is what §8.5 asks for."
	}

	loaner := model.PreShowCheck{
		ID: "loaner_kit_ready", Title: "Loaner devices and wired headphones are charged and ready",
		Section: "§2, §12.5", Source: model.SourceStaff, Blocking: false,
		Detail: "At least one charged loaner phone with WIRED headphones, plus the fallback receivers if " +
			"the venue has them.",
		Remedy: "A patron with no usable device has no access, whatever this appliance reports. Wired " +
			"headphones matter for HI specifically: Bluetooth adds 100-300 ms on top of the budget.",
	}
	return []model.PreShowCheck{patronNet, adminHidden, cable, loaner}
}

func passFail(ok bool) model.CheckState {
	if ok {
		return model.CheckPass
	}
	return model.CheckFail
}
