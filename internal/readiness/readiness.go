// Package readiness assembles the composite dashboard verdict (buildplan.md §10.6,
// §8.2, §8.3): the two INDEPENDENT top-line verdicts (caption_ready, audio_ready)
// plus the loud-warning set. Captions and audio are computed independently — the
// system can have valid captions and a failed VI feed, or clean HI audio and no
// captions (§2 asymmetry, guardrail #7 no-silent-degradation).
package readiness

import (
	"strings"

	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/verify"
)

// Network names the interface/allowlist facts surfaced on the dashboard.
type Network struct {
	TheaterInterface string
	ClientInterface  string
	// AllowedServer is the CONFIGURED cinema server pinned for this auditorium
	// (§8.2 "Allowed cinema server", §10.5) — NOT the connected server's own
	// claim about itself, which cannot contradict itself and so can never expose
	// a wrong-room error (§8.5).
	AllowedServer string
	// ConnectedServer is what the connected server announced.
	ConnectedServer string
	// IdentityStrength labels how the pinned identity is established (§10.5:
	// a composite identity must be labelled as such in the UI).
	IdentityStrength string
	// IdentityCheck is the OUTCOME of the §8.5 wrong-room guard (acsp.IdentityState
	// as a string). Empty means the guard has not reported at all, which renders
	// as "not checked" — never as a pass.
	IdentityCheck string
}

// Deploy is the §12 posture in force, as far as the dashboard has to render it.
// §8.4 permits an admin surface on the patron network ONLY in Lab and Booth
// Diagnostic and ONLY behind a prominent warning banner, so the banner is a
// dashboard obligation, not merely a profile attribute.
type Deploy struct {
	Profile model.FirewallProfile
	Title   string
	// WarningBanner is deploy.Profile.WarningBanner: this posture must not be
	// operated without a visible banner saying what it is.
	WarningBanner bool
	// AdminOnPatronNet is the MEASURED §8.4 fact — the admin surface's bind
	// address is reachable from the patron network. verify.Report.AdminIsolated
	// cannot answer this: it reports whether the binding meets the active
	// profile's requirement, and Lab and Booth Diagnostic have no such
	// requirement, so it reads "isolated" in precisely the case §8.4 exists for.
	AdminOnPatronNet model.Tri
}

// Clients carries §8.2's connected-client counts and the D11 per-profile cap in
// force in the fan-out.
type Clients struct {
	Caption int
	Max     int
}

// LatencyBudgets are the per-stream over-threshold limits (§6.7 / D10).
type LatencyBudgets struct {
	HIMaxMS  int
	VINMaxMS int
}

// DefaultLatencyBudgets returns the aspirational §6.7 budgets.
func DefaultLatencyBudgets() LatencyBudgets { return LatencyBudgets{HIMaxMS: 150, VINMaxMS: 250} }

// Inputs is everything the composite verdict is assembled from (§10.6:
// caption-link state, per-input audio state, firewall/verification state, and
// network/interface assignment — plus the §8.3 loud-warning set and the §3.5
// per-process liveness). It is a struct rather than a parameter list because the
// dashboard grows facts; a caller that does not have one leaves it zero and the
// report says nothing about it rather than guessing.
type Inputs struct {
	Link    model.CaptionLinkState
	Audio   []model.AudioStreamState
	Verify  verify.Report
	Network Network
	Budgets LatencyBudgets
	// CaptionWarnings are the live caption-path warnings (§8.3). They come from
	// CaptionWarnings.Snapshot, i.e. from conditions the ACSP link actually
	// raised — guardrail #7 applies to the caption path exactly as it does to
	// audio.
	CaptionWarnings []model.Warning
	// HelperWarnings are conditions core observed about the supervised helpers
	// themselves — a refused command, a rejected input profile — which no
	// snapshot of measurements can be derived from.
	HelperWarnings []model.Warning
	// Processes is the measured liveness of the supervised processes (§3.5).
	Processes []model.ProcessLiveness
	Clients   Clients
	// Versions is the persisted §10.8 record, surfaced so a tech can see what
	// changed since this last worked.
	Versions model.SystemConfigVersion
	// Deploy is the §12 posture and its §8.4 banner obligation.
	Deploy Deploy
	// Timeline is §8.2's play/pause/stop row, MEASURED from the caption
	// controller's reference engine — the thing the server's transport state
	// actually drives — rather than inferred from the handshake state.
	Timeline model.TimelineState
	// CueVisible is §8.2's "Current cue: visible / none", taken from the fan-out
	// hub. VISIBILITY ONLY: the cue text never leaves the hub for a diagnostic
	// surface, a log or a persisted record (§9.4 caption.cue.visible_no_text).
	CueVisible model.Tri
	// Discovery and Support are §8.2's last two rows. Both are Tri so an
	// un-observed one renders "not measured" rather than a reassuring "Off".
	Discovery model.Tri
	Support   model.Tri
	// DriftBudgetMS is the §5.5 / D9 warn budget the link's drift estimate is
	// judged against, so the dashboard can show the number against its limit.
	DriftBudgetMS int64
}

// Assemble builds the composite readiness from the measured inputs.
func Assemble(in Inputs) model.SystemReadiness {
	budgets := in.Budgets
	if budgets.HIMaxMS == 0 {
		budgets = DefaultLatencyBudgets()
	}
	rep := in.Verify
	r := model.SystemReadiness{
		CaptionReady: in.Link.CaptionReady(),
		AudioReady:   audioReady(in.Audio),
		CaptionLink:  in.Link,
		AudioInputs:  in.Audio,

		ForwardingDisabled:   rep.ForwardingDisabledV4 && rep.ForwardingDisabledV6,
		FirewallActive:       rep.FirewallActive,
		FirewallIntrospected: rep.FirewallIntrospected,
		BridgesPresent:       rep.BridgesPresent,
		AdminIsolated:        rep.AdminIsolated,
		DefaultCreds:         !rep.NoDefaultCreds,
		SSHDisabled:          rep.SSHCorrect,

		TheaterInterface: in.Network.TheaterInterface,
		ClientInterface:  in.Network.ClientInterface,
		AllowedServer:    in.Network.AllowedServer,
		ConnectedServer:  in.Network.ConnectedServer,
		IdentityStrength: in.Network.IdentityStrength,
		IdentityCheck:    in.Network.IdentityCheck,

		DeploymentProfile: in.Deploy.Profile,
		DeploymentTitle:   in.Deploy.Title,
		DeploymentBanner:  deploymentBanner(in.Deploy),
		AdminOnPatronNet:  in.Deploy.AdminOnPatronNet,

		ServerConnected:      model.TriOf(in.Link.ServerConnected()),
		Timeline:             in.Timeline,
		CueVisible:           in.CueVisible,
		CaptionDriftBudgetMS: in.DriftBudgetMS,

		AudioStreams:          summarizeAudio(in.Audio),
		ConnectedAudioClients: audioClients(in.Audio),
		DiscoveryMode:         in.Discovery,
		SupportSession:        in.Support,

		Processes:               in.Processes,
		ConnectedCaptionClients: in.Clients.Caption,
		MaxConcurrentClients:    in.Clients.Max,
		Versions:                in.Versions,
	}

	// Loud-warning set (§8.3): the security warnings, then the caption-link ones,
	// then the audio ones, then the process-liveness ones. The dashboard must
	// shout, not whisper.
	warnings := append([]model.Warning{}, rep.Warnings...)
	warnings = append(warnings, postureWarnings(rep)...)
	warnings = append(warnings, in.CaptionWarnings...)
	warnings = append(warnings, in.HelperWarnings...)
	warnings = append(warnings, audioWarnings(in.Audio, budgets)...)
	warnings = append(warnings, processWarnings(in.Processes)...)
	warnings = append(warnings, supportWarnings(in.Support)...)
	r.Warnings = warnings
	return r
}

// deploymentBanner is §8.4's prominent warning banner. The profiles that carry
// WarningBanner are exactly the two that are not for public installs; when the
// admin surface really is reachable from the patron side the banner says THAT,
// because "admin on the patron network" is the specific condition §8.4 allows
// only here and only behind a banner. An empty string means no banner is due.
//
// The admin-on-patron fact comes from Deploy, not from verify's AdminIsolated:
// AdminIsolated answers "does this binding meet the active profile's
// requirement", and these two profiles impose none — so it reads true in exactly
// the case this branch has to catch.
func deploymentBanner(d Deploy) string {
	if !d.WarningBanner {
		return ""
	}
	title := d.Title
	if title == "" {
		title = string(d.Profile)
	}
	if d.AdminOnPatronNet == model.TriYes {
		return title + " profile: the admin surface is reachable from the PATRON network. " +
			"§8.4 permits this only in Lab and Booth Diagnostic — never on a public install."
	}
	return title + " profile: diagnostic posture, not a public install. " +
		"Switch to Patron-Facing or Low-Cost Replacement before serving an audience."
}

// supportWarnings raises §8.3's last condition — "debug capture / support mode
// active". §9.4 makes support mode outbound-only, time-limited and audited, and
// "visibly indicated"; the indication is this warning. Nothing raised it before,
// so the code existed and the condition did not.
func supportWarnings(support model.Tri) []model.Warning {
	if support != model.TriYes {
		return nil
	}
	return []model.Warning{{
		Code:     model.WarnDebugCaptureActive,
		Message:  "A support session is open — it is time-limited and audited, but close it before the show",
		Severity: model.SeverityWarning,
	}}
}

// summarizeAudio derives §8.2's per-stream audio rows from what the media
// process measured (§10.2). Nothing here invents a value: an input the media
// process never reported produces no row at all, and the dashboard says so.
func summarizeAudio(audio []model.AudioStreamState) []model.AudioStreamSummary {
	if len(audio) == 0 {
		return nil
	}
	out := make([]model.AudioStreamSummary, 0, len(audio))
	for _, a := range audio {
		s := model.AudioStreamSummary{
			StreamType:           a.StreamType,
			InputPresent:         model.TriOf(a.Present),
			StreamActive:         streamActive(a),
			Quality:              a.Status,
			Clients:              a.ConnectedClients,
			LatencyMeasured:      a.LatencyMeasured,
			EncodePathEstimateMS: a.LatencyMSEst,
			EncoderState:         a.EncoderState,
		}
		// §8.2's "Estimated audio latency" is a glass-to-ear figure (§6.7 / D10).
		// Only an END-TO-END measurement may fill it; the encode-path estimate is
		// reported under its own name and never promoted into this field, which
		// is precisely the substitution the second fabrication made.
		if a.LatencyMeasured {
			s.LatencyMS = a.LatencyMSEst
		}
		out = append(out, s)
	}
	return out
}

// streamActive derives §8.2's "HI stream active / VI stream active".
//
// A stream is active only when something is genuinely encoding AND there is a
// transport to carry it. Neither of the two states this build can reach is that,
// so today the honest answers are "no" and "not measured" — and an encoder state
// this build does not recognise is NOT evidence of a live stream (guardrail #7).
func streamActive(a model.AudioStreamState) model.Tri {
	switch a.EncoderState {
	case model.EncoderStateUnavailable:
		// No encoder ran: nothing can reach a patron however clean the input is.
		return model.TriNo
	case model.EncoderStateReadyNoTransport:
		// A real encode was proven locally, but there is no WebRTC subscriber
		// transport (BLOCKERS B2/B4), so no patron is receiving this stream.
		return model.TriNo
	default:
		// "unknown" (the encode path has not been exercised) and anything a later
		// media build introduces: not measured, never a green tick.
		return model.TriUnknown
	}
}

// audioClients totals §8.2's "Connected audio clients" across the streams.
func audioClients(audio []model.AudioStreamState) int {
	n := 0
	for _, a := range audio {
		n += a.ConnectedClients
	}
	return n
}

// postureWarnings adds the security findings the verification pass reports as
// facts but has no §8.3 code for.
func postureWarnings(rep verify.Report) []model.Warning {
	var ws []model.Warning
	// "Active but nobody read the ruleset" is a different fact from "inactive",
	// and verify deliberately does not reuse WarnFirewallInactive for it (that
	// would tell an operator the firewall is DOWN when it may be up). Surface it
	// as its own condition: the dashboard must not present an unverified claim as
	// a verified one (§9.3, D17).
	if rep.FirewallActive && !rep.FirewallIntrospected {
		ws = append(ws, model.Warning{
			Code:     model.WarnFirewallUnverified,
			Message:  "The firewall is reported active but the loaded ruleset was never read — the posture is asserted, not verified",
			Severity: model.SeverityWarning,
		})
	}
	return ws
}

// processWarnings raises §3.5's per-process liveness failures. Core supervises
// the media and firewall helpers; a helper that is not answering is a named
// fault, not an unexplained "audio not ready" or "firewall inactive".
func processWarnings(procs []model.ProcessLiveness) []model.Warning {
	var ws []model.Warning
	for _, p := range procs {
		if p.Alive {
			continue
		}
		ws = append(ws, model.Warning{
			Code:     model.WarnProcessUnreachable,
			Message:  p.Name + " is not answering on the loopback socket — restart it before the show (see systemd status)",
			Severity: model.SeverityCritical,
		})
	}
	return ws
}

// audioReady reports whether the audio path is serviceable: at least one stream is
// usable and no configured stream has Failed (a failed stream is a loud problem,
// not silently ignored). No configured audio → not ready (honest: there is none).
func audioReady(audio []model.AudioStreamState) bool {
	if len(audio) == 0 {
		return false
	}
	anyUsable := false
	for _, a := range audio {
		if a.Status == model.QualFailed {
			return false
		}
		if a.Status.Usable() {
			anyUsable = true
		}
	}
	return anyUsable
}

// audioWarnings derives the §8.3 audio loud-warnings from the per-stream states.
func audioWarnings(audio []model.AudioStreamState, budgets LatencyBudgets) []model.Warning {
	var ws []model.Warning
	for _, a := range audio {
		label := string(a.StreamType)
		if a.Status == model.QualFailed && (a.Silence || reasonHas(a.Reasons, "silent")) {
			ws = append(ws, model.Warning{
				Code: model.WarnAudioSilentInPlayback, Severity: model.SeverityCritical,
				Message: label + " input is silent during playback — contact theater staff",
			})
		}
		// The mapping fault §8.3 names is specifically "a configured stereo
		// mapping impossible on a detected mono input". Match that reason and
		// nothing else: a bare "requires" would raise a CRITICAL mapping fault for
		// any future reason string that happens to contain the word.
		if reasonHas(a.Reasons, "requires two channels") {
			ws = append(ws, model.Warning{
				Code: model.WarnImpossibleMapping, Severity: model.SeverityCritical,
				Message: label + " channel mapping is impossible on the detected input (caught before show)",
			})
		}
		// §6.6 reports device connect/disconnect; §8.3 has to shout the cause. A
		// failed stream whose device is not connected is "the dongle fell out",
		// not "the input is silent" — the booth needs the actual cause named.
		if a.Status == model.QualFailed && !a.DeviceConnected {
			ws = append(ws, model.Warning{
				Code: model.WarnAudioDeviceDisconnected, Severity: model.SeverityCritical,
				Message: label + " capture device is not connected — check the input cable/adapter",
			})
		}
		// An unavailable encoder means nothing is being encoded for this stream,
		// however clean the input measures (§6.6 "encoder state"). Without this,
		// a perfectly qualified input reads as ready while patrons get silence.
		if a.EncoderState == model.EncoderStateUnavailable {
			ws = append(ws, model.Warning{
				Code: model.WarnAudioEncoderUnavailable, Severity: model.SeverityCritical,
				Message: label + " has no working audio encoder — no audio can reach patrons",
			})
		}
		if a.Clipping {
			ws = append(ws, model.Warning{
				Code: model.WarnAudioClipping, Severity: model.SeverityWarning,
				Message: label + " audio is clipping — reduce source level or add the inline attenuator",
			})
		}
		max := budgets.VINMaxMS
		if a.StreamType == model.StreamHI {
			max = budgets.HIMaxMS
		}
		// One-sided test: LatencyMSEst is the appliance-side ENCODE-PATH estimate
		// only (§6.7 / D10 budgets are glass-to-ear), so it can prove the budget
		// is blown but never that it is met. Re-base once B3 measures end to end.
		if a.LatencyMSEst > max && max > 0 {
			ws = append(ws, model.Warning{
				Code: model.WarnAudioLatencyOver, Severity: model.SeverityWarning,
				Message: label + " estimated latency exceeds the budget for this stream",
			})
		}
	}
	return ws
}

func reasonHas(reasons []string, sub string) bool {
	for _, r := range reasons {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}
