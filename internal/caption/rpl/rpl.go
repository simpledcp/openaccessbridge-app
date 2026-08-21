// Package rpl parses the ST 430-11 Auxiliary Resource Presentation List (RPL) —
// the XML the cinema server provides (or points to) that enumerates, per reel, the
// auxiliary resources including the timed-text assets for captions/subtitles
// (buildplan.md §5.3).
//
// PROVISIONAL: the concrete element grammar here is built from public summaries of
// ST 430-11 and the ST 429-12 track-type vocabulary, NOT the licensed standard.
// The simulator (internal/sim) emits the same provisional grammar so parser and
// simulator agree; the Phase 0A wire contract replaces both behind this package's
// API without changing its callers.
package rpl

import (
	"sort"
	"strings"

	"github.com/simpledcp/captionbridge/internal/caption/timedtext"
	"github.com/simpledcp/captionbridge/internal/model"
)

// RPL is a parsed Auxiliary Resource Presentation List, keyed (by the caller) by
// its Playout ID.
type RPL struct {
	ID        string
	PlayoutID string
	Reels     []Reel
}

// Reel is one reel's auxiliary resources. An empty Resources list is legitimate
// (reels/compositions without captions) and must render "no captions for this
// segment," not an error (§5.2 item 5).
//
// EditRate/EntryPointUnits/DurationUnits carry the REEL-level timing when the RPL
// declares it. They matter because an uncaptioned reel carries no resources at
// all, so a duration derived from resource durations alone would be 0 ms and every
// following reel would anchor at the wrong composition offset (§5.2 item 5 /
// §5.3 "map from timeline segments → reel resources"). PROVISIONAL element names —
// see parse.go.
type Reel struct {
	ReelNumber      int
	Resources       []Resource
	EditRate        timedtext.EditRate
	EntryPointUnits int64
	DurationUnits   int64
}

// Resource is a single auxiliary resource. For accessibility we care about the
// closed timed-text tracks; other resource types are retained but flagged.
type Resource struct {
	ID                     string
	RawType                string // verbatim ST 429-12 track type string from the RPL
	Kind                   model.TrackKind
	Language               string
	Locator                string // raw locator from the RPL (resolved by the resolver)
	MimeType               string
	EditRate               timedtext.EditRate
	EntryPointUnits        int64
	IntrinsicDurationUnits int64
}

// DurationMS returns the resource's intrinsic duration in milliseconds.
func (r Resource) DurationMS() int64 { return unitsToMS(r.IntrinsicDurationUnits, r.EditRate) }

// EntryPointMS returns the resource entry point in milliseconds — the offset into
// the asset at which the reel actually starts, which cue anchoring must subtract
// (§5.4 "cue times relative to the reel/timeline").
func (r Resource) EntryPointMS() int64 { return unitsToMS(r.EntryPointUnits, r.EditRate) }

// unitsToMS converts editable units to milliseconds at the given edit rate. A
// missing/zero rate or a non-positive count yields 0 (unknown), never a guess.
func unitsToMS(units int64, er timedtext.EditRate) int64 {
	ps := er.PerSecond()
	if ps <= 0 || units <= 0 {
		return 0
	}
	return int64(float64(units)*1000.0/ps + 0.5)
}

// IsTimedText reports whether the resource is a caption/subtitle timed-text track
// (the kind CaptionBridge consumes), as opposed to other auxiliary resource types.
func (r Resource) IsTimedText() bool {
	switch r.Kind {
	case model.TrackClosedCaption, model.TrackClosedSubtitle, model.TrackMainSubtitle, model.TrackMainCaption:
		return true
	default:
		return false
	}
}

// kindFromType maps an ST 429-12 track-type string to the internal TrackKind.
func kindFromType(t string) model.TrackKind {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "closedcaption", "closed-caption", "closed_caption", "cc":
		return model.TrackClosedCaption
	case "closedsubtitle", "closed-subtitle", "closed_subtitle":
		return model.TrackClosedSubtitle
	case "mainsubtitle", "subtitle", "main-subtitle":
		return model.TrackMainSubtitle
	case "maincaption", "caption", "main-caption":
		return model.TrackMainCaption
	default:
		return model.TrackUnknown
	}
}

// BestKindFor returns the most accessible track kind carried for a language, or
// TrackUnknown when the language is not present.
//
// "Most accessible" is model.TrackKind.AccessibilityRank: a ClosedCaption track
// beats a subtitle track because it carries speaker names, sound effects and
// music cues — the information a deaf or hard-of-hearing patron is relying on.
func (r *RPL) BestKindFor(language string) model.TrackKind {
	best := model.TrackUnknown
	found := false
	for _, reel := range r.Reels {
		for _, res := range reel.Resources {
			if !res.IsTimedText() || res.Language != language {
				continue
			}
			if !found || res.Kind.AccessibilityRank() < best.AccessibilityRank() {
				best, found = res.Kind, true
			}
		}
	}
	return best
}

// LanguagesByAccessibility returns the distinct timed-text languages ordered by
// how accessible their best track is, ties broken by first-seen order.
//
// This is D8 steps (3) and (4) — "first ClosedCaption track, then first subtitle
// track" — which were previously collapsed into plain RPL order. The difference
// shows on a show that carries both: with no venue preference configured, RPL
// order could put a subtitle track first, so the appliance would default to
// subtitles and eagerly fetch them while a closed-caption track sat in the same
// playlist. A deaf patron would be served the track that omits exactly what they
// need, on a show that carried the right one.
func (r *RPL) LanguagesByAccessibility() []string {
	langs := r.Languages()
	sort.SliceStable(langs, func(i, j int) bool {
		return r.BestKindFor(langs[i]).AccessibilityRank() < r.BestKindFor(langs[j]).AccessibilityRank()
	})
	return langs
}

// Languages returns the distinct languages of the timed-text resources, in first-
// seen order.
func (r *RPL) Languages() []string {
	seen := map[string]bool{}
	var out []string
	for _, reel := range r.Reels {
		for _, res := range reel.Resources {
			if res.IsTimedText() && res.Language != "" && !seen[res.Language] {
				seen[res.Language] = true
				out = append(out, res.Language)
			}
		}
	}
	return out
}

// ReelOffsetMS returns the composition-timeline start offset (ms) of the reel at
// index i, computed as the cumulative duration of the preceding reels. Used to map
// reel-relative cue times onto the composition timeline (§5.3 / §5.5).
func (r *RPL) ReelOffsetMS(i int) int64 {
	var off int64
	for j := 0; j < i && j < len(r.Reels); j++ {
		off += r.Reels[j].DurationMS()
	}
	return off
}

// ReelIndexAt returns the index of the reel containing the RPL-relative timeline
// position posMS. This is the inverse of ReelOffsetMS and is what lets the ACS
// honor the timeline — fetching the current and next reel(s) and skipping reels
// already past, which matters most on a mid-show reconnect (§5.3). A position
// before the composition clamps to the first reel and one past the end clamps to
// the last; a reel with an unknown (0 ms) duration cannot contain a position and
// is stepped over.
func (r *RPL) ReelIndexAt(posMS int64) int {
	if len(r.Reels) == 0 {
		return 0
	}
	if posMS <= 0 {
		return 0
	}
	var off int64
	for i, re := range r.Reels {
		d := re.DurationMS()
		if d > 0 && posMS < off+d {
			return i
		}
		off += d
	}
	return len(r.Reels) - 1
}

// TotalDurationMS returns the composition duration the RPL covers, or 0 when no
// reel declares a duration. Used to place an RPL on the timeline as a segment.
func (r *RPL) TotalDurationMS() int64 {
	var total int64
	for _, re := range r.Reels {
		total += re.DurationMS()
	}
	return total
}

// DurationMS returns the reel's duration on the composition timeline. A
// reel-level <Duration> wins when the RPL declares one; otherwise we fall back to
// the longest resource duration, which is only meaningful for a reel that actually
// carries captions — an uncaptioned reel legitimately has none (§5.2 item 5), and
// its declared reel duration is the only thing that keeps the following reels
// anchored correctly (§5.3).
func (re Reel) DurationMS() int64 {
	if ms := unitsToMS(re.DurationUnits, re.EditRate); ms > 0 {
		return ms
	}
	var max int64
	for _, res := range re.Resources {
		if d := res.DurationMS(); d > max {
			max = d
		}
	}
	return max
}

// EntryPointMS returns the reel-level entry point in milliseconds (0 when absent).
func (re Reel) EntryPointMS() int64 { return unitsToMS(re.EntryPointUnits, re.EditRate) }

// TrimMS returns the offset into the assets at which this reel starts, preferring
// the resource's own entry point and falling back to the reel's. Cue times in a
// timed-text asset are asset-relative, so the trim is subtracted when anchoring
// them onto the composition timeline (§5.3 output: "reel/timeline anchoring").
func (re Reel) TrimMS(res Resource) int64 {
	if t := res.EntryPointMS(); t > 0 {
		return t
	}
	return re.EntryPointMS()
}

// SelectResource returns the best timed-text resource in a reel for the requested
// language, applying the accessibility preference (ClosedCaption over
// ClosedSubtitle over on-screen, §5.4). Returns false if none match.
func (re Reel) SelectResource(language string) (Resource, bool) {
	best := Resource{}
	found := false
	for _, res := range re.Resources {
		if !res.IsTimedText() || res.Language != language {
			continue
		}
		if !found || res.Kind.AccessibilityRank() < best.Kind.AccessibilityRank() {
			best = res
			found = true
		}
	}
	return best, found
}
