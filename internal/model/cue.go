// Package model holds the shared, transient in-memory data shapes and the
// persisted configuration shapes described in buildplan.md §10.
//
// INVARIANT (guardrail #5, §9.4): caption text and audio samples are content
// and MUST NOT be persisted by default. The cue and audio types in this package
// are transient (memory/tmpfs only). The persisted types (AuditoriumProfile,
// SystemConfigVersion) deliberately carry NO caption text, audio, asset paths
// that embed titles, or show identifiers beyond what configuration needs.
package model

// TrackKind distinguishes the ST 429-12 closed text-track types from on-screen
// subtitle tracks. The distinction is an accessibility decision, not cosmetics
// (§5.4): ClosedCaption carries speaker IDs, sound effects, and music cues for
// deaf/HoH patrons; subtitles typically carry only translated dialogue.
type TrackKind string

const (
	// TrackClosedCaption is an off-screen ST 429-12 ClosedCaption track — the
	// accessibility-grade track OpenAccessBridge prefers.
	TrackClosedCaption TrackKind = "closed_caption"
	// TrackClosedSubtitle is an off-screen ST 429-12 ClosedSubtitle track.
	TrackClosedSubtitle TrackKind = "closed_subtitle"
	// TrackMainSubtitle is an on-screen subtitle track (ST 428-7); generally
	// not consumed for accessibility but recognized so it can be deprioritized.
	TrackMainSubtitle TrackKind = "main_subtitle"
	// TrackMainCaption is an on-screen caption track.
	TrackMainCaption TrackKind = "main_caption"
	// TrackUnknown is used when the track type cannot be determined.
	TrackUnknown TrackKind = "unknown"
)

// AccessibilityRank returns a preference rank for accessibility selection: lower
// is preferred. ClosedCaption (full accessibility info) outranks ClosedSubtitle,
// which outranks on-screen tracks (§5.4 track-selection model).
func (k TrackKind) AccessibilityRank() int {
	switch k {
	case TrackClosedCaption:
		return 0
	case TrackClosedSubtitle:
		return 1
	case TrackMainCaption:
		return 2
	case TrackMainSubtitle:
		return 3
	default:
		return 4
	}
}

// IsCaption reports whether the track is caption-grade (carries speaker/SFX/music
// info) rather than subtitle-grade. Drives the patron-facing "Closed captions"
// vs "Subtitles available" label so a caption-reliant patron is not misled.
func (k TrackKind) IsCaption() bool {
	return k == TrackClosedCaption || k == TrackMainCaption
}

// Cue is the normalized, timeline-relative caption cue (§10.1). It is transient:
// it lives in memory and is purged at show end; it is never persisted to disk
// and never written to logs.
type Cue struct {
	// ResourceID is an opaque identifier for the source timed-text resource.
	ResourceID string `json:"resource_id"`
	// Language is the BCP-47 / RFC 5646 language tag of the cue (e.g. "en-US").
	Language string `json:"language"`
	// Kind is the track kind this cue came from (caption vs subtitle).
	Kind TrackKind `json:"kind,omitempty"`
	// StartMS / EndMS are timeline-relative milliseconds (§5.4): a single
	// timeline reference, after edit-rate/timecode conversion.
	StartMS int64 `json:"start_ms"`
	EndMS   int64 `json:"end_ms"`
	// Text is the displayable caption text (already line-joined). CONTENT —
	// never logged, never persisted.
	Text string `json:"text"`
	// Style and Position are opaque pass-through fields (§5.4): carried but not
	// rendered in early phases. Kept as raw maps so a later fidelity pass can
	// interpret them without a parser rewrite.
	Style    map[string]string `json:"style,omitempty"`
	Position map[string]string `json:"position,omitempty"`
}

// Duration returns the cue's on-screen duration in milliseconds.
func (c Cue) Duration() int64 { return c.EndMS - c.StartMS }

// ContainsMS reports whether the given timeline position (ms) falls within the
// cue's [start,end) interval.
func (c Cue) ContainsMS(posMS int64) bool {
	return posMS >= c.StartMS && posMS < c.EndMS
}
