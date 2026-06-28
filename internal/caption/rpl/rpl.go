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
type Reel struct {
	ReelNumber int
	Resources  []Resource
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
func (r Resource) DurationMS() int64 {
	ps := r.EditRate.PerSecond()
	if ps <= 0 || r.IntrinsicDurationUnits <= 0 {
		return 0
	}
	return int64(float64(r.IntrinsicDurationUnits)*1000.0/ps + 0.5)
}

// EntryPointMS returns the resource entry point in milliseconds.
func (r Resource) EntryPointMS() int64 {
	ps := r.EditRate.PerSecond()
	if ps <= 0 || r.EntryPointUnits <= 0 {
		return 0
	}
	return int64(float64(r.EntryPointUnits)*1000.0/ps + 0.5)
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

// DurationMS returns the reel's duration, taken as the max resource duration.
func (re Reel) DurationMS() int64 {
	var max int64
	for _, res := range re.Resources {
		if d := res.DurationMS(); d > max {
			max = d
		}
	}
	return max
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
