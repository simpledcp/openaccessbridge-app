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
	AllowedServer    string
}

// LatencyBudgets are the per-stream over-threshold limits (§6.7 / D10).
type LatencyBudgets struct {
	HIMaxMS  int
	VINMaxMS int
}

// DefaultLatencyBudgets returns the aspirational §6.7 budgets.
func DefaultLatencyBudgets() LatencyBudgets { return LatencyBudgets{HIMaxMS: 150, VINMaxMS: 250} }

// Assemble builds the composite readiness from the caption-link state, the audio
// stream states, the security verification report, and the network facts.
func Assemble(link model.CaptionLinkState, audio []model.AudioStreamState, rep verify.Report, net Network, budgets LatencyBudgets) model.SystemReadiness {
	if budgets.HIMaxMS == 0 {
		budgets = DefaultLatencyBudgets()
	}
	r := model.SystemReadiness{
		CaptionReady: link.CaptionReady(),
		AudioReady:   audioReady(audio),
		CaptionLink:  link,
		AudioInputs:  audio,

		ForwardingDisabled: rep.ForwardingDisabledV4 && rep.ForwardingDisabledV6,
		FirewallActive:     rep.FirewallActive,
		BridgesPresent:     rep.BridgesPresent,
		AdminIsolated:      rep.AdminIsolated,
		DefaultCreds:       !rep.NoDefaultCreds,
		SSHDisabled:        rep.SSHCorrect,

		TheaterInterface: net.TheaterInterface,
		ClientInterface:  net.ClientInterface,
		AllowedServer:    net.AllowedServer,
	}

	// Loud-warning set (§8.3): start with the security warnings, then add the
	// audio ones. The dashboard must shout, not whisper.
	warnings := append([]model.Warning{}, rep.Warnings...)
	warnings = append(warnings, audioWarnings(audio, budgets)...)
	r.Warnings = warnings
	return r
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
		if reasonHas(a.Reasons, "requires two channels") || reasonHas(a.Reasons, "requires") {
			ws = append(ws, model.Warning{
				Code: model.WarnImpossibleMapping, Severity: model.SeverityCritical,
				Message: label + " channel mapping is impossible on the detected input (caught before show)",
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
