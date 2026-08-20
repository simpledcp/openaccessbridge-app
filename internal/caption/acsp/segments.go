package acsp

import (
	"crypto/sha256"
	"encoding/hex"
	"math"

	"github.com/simpledcp/captionbridge/internal/caption/rpl"
)

// Timeline-segment keying for RPLs — §5.3 ("maintain a map from timeline segments
// → reel resources") and §5.2 item 4 ("Disambiguate using timeline position, not
// Playout ID alone").
//
// A server may send several Set RPL Location requests on one connection: the
// trailer pack and the feature are different compositions on one timeline. Keeping
// only the latest RPL makes the feature's captions render during the trailer, and
// keying by Playout ID alone breaks on the servers that send Playout ID = 0 for
// every uncaptioned composition. So each RPL is stored as a timeline SEGMENT and
// the active one is chosen by position.
//
// PROVISIONAL (D7, BLOCKER B1): the public material does not say how a server
// anchors each Set RPL Location onto the composition timeline, so the placement
// rules below are the model this build commits to, and the ones Phase 0A confirms:
//
//  1. RPLs are stored in arrival order as consecutive segments; the first RPL of a
//     connection presents the composition from position 0.
//  2. Exception — a mid-show reconnect in which the server sends only the segment
//     now playing: if the current timeline anchor is already past that RPL's whole
//     duration, the segment starts at the anchor instead of at 0.
//  3. A re-sent RPL — identical bytes, or the same Playout ID listing the same
//     resources — replaces its segment in place rather than appending a duplicate.
//     A bare Playout ID match is never enough: a repeated ID must never look like a
//     loop-back, and two different compositions do share Playout ID 0 (§5.2 item 4).
//  4. The active segment is chosen by TIMELINE POSITION; the Playout ID only
//     breaks ties between overlapping segments and only when it is unambiguous.
//     This is deliberate: §5.2 item 4 warns that Playout IDs are not dependably
//     unique, so a stale or zero ID must never override an advancing timeline.
//  5. Retention is capped (maxRetainedSegments): an abandoned RPL — a new show
//     loaded without the Terminate Lease the §5.2 new-show sequence calls for — is
//     not held forever, and the fetched assets behind it are evicted at the
//     boundary (guardrail #5).
//
// Known limitation, recorded rather than papered over: a server that loads a new
// show WITHOUT Terminate Lease *and* restarts the timeline at 0 is, on the material
// we have, indistinguishable from a rewind into the first segment. Position stays
// the authority (rule 4) and Terminate Lease stays the show boundary (§5.2
// new-show-load). Phase 0A closes this.

// maxRetainedSegments bounds how many RPL segments are held at once. RPL structs
// are small metadata (IDs, locators, languages) — the fetched timed text they point
// at is what is purged at boundaries — but the set must still be bounded so a
// server that never sends Terminate Lease cannot grow it without limit.
const maxRetainedSegments = 8

// rplSegment binds one parsed RPL to the composition-timeline span it presents.
type rplSegment struct {
	startMS int64
	durMS   int64 // 0 = unknown → open-ended
	playout string
	fp      string // fingerprint of the source bytes (re-send detection)
	doc     *rpl.RPL
}

// endMS is the exclusive end of the segment; an unknown duration is open-ended.
func (s rplSegment) endMS() int64 {
	if s.durMS <= 0 {
		return math.MaxInt64
	}
	return s.startMS + s.durMS
}

func (s rplSegment) contains(posMS int64) bool {
	return posMS >= s.startMS && posMS < s.endMS()
}

// scope is the segment's cache namespace: stable across re-selection and unique to
// this RPL's content, so a resource ID reused by another show cannot collide.
func (s rplSegment) scope() string {
	return "p" + s.playout + "#" + s.fp
}

// fingerprint hashes the RPL source bytes. Only a short digest is kept — it is used
// for identity and is safe to log (it carries no content).
func fingerprint(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// putSegment stores a parsed RPL as a timeline segment and returns its index,
// replacing a re-sent RPL in place (rule 3) or chaining a new one after the last
// (rules 1-2).
func (m *Machine) putSegment(doc *rpl.RPL, fp string) int {
	if i := m.findSegment(doc, fp); i >= 0 {
		m.segments[i].doc = doc
		m.segments[i].durMS = doc.TotalDurationMS()
		m.segments[i].fp = fp
		return i
	}
	seg := rplSegment{startMS: m.nextSegmentStart(doc), durMS: doc.TotalDurationMS(),
		playout: doc.PlayoutID, fp: fp, doc: doc}
	m.segments = append(m.segments, seg)
	idx := len(m.segments) - 1
	if len(m.segments) > maxRetainedSegments {
		drop := len(m.segments) - maxRetainedSegments
		m.segments = append([]rplSegment(nil), m.segments[drop:]...)
		idx -= drop
		m.activeSeg -= drop
		if m.activeSeg < 0 {
			m.activeSeg = -1
		}
	}
	return idx
}

// findSegment locates a segment the incoming RPL is a re-send of: identical bytes,
// or the same Playout ID listing the same resources (a re-serialization of the same
// RPL). A bare Playout ID match is never enough — two different compositions
// legitimately share Playout ID 0 (§5.2 item 4), and treating the second as a
// re-send of the first would drop a real timeline segment.
func (m *Machine) findSegment(doc *rpl.RPL, fp string) int {
	for i, s := range m.segments {
		if s.fp == fp {
			return i
		}
	}
	if doc.PlayoutID == "" {
		return -1
	}
	for i, s := range m.segments {
		if s.playout == doc.PlayoutID && sameResources(s.doc, doc) {
			return i
		}
	}
	return -1
}

// sameResources reports whether two RPLs list the same resources reel for reel.
func sameResources(a, b *rpl.RPL) bool {
	if a == nil || b == nil || len(a.Reels) != len(b.Reels) {
		return false
	}
	for i := range a.Reels {
		ar, br := a.Reels[i].Resources, b.Reels[i].Resources
		if len(ar) != len(br) {
			return false
		}
		for j := range ar {
			if ar[j].ID != br[j].ID || ar[j].Locator != br[j].Locator {
				return false
			}
		}
	}
	return true
}

// nextSegmentStart places an incoming RPL on the composition timeline (rules 1-2).
func (m *Machine) nextSegmentStart(doc *rpl.RPL) int64 {
	if len(m.segments) == 0 {
		total := doc.TotalDurationMS()
		if total > 0 && m.timelineMS >= total {
			// Mid-show reconnect where the server sent only the segment now
			// playing: it cannot be the composition from zero.
			return m.timelineMS
		}
		return 0
	}
	last := m.segments[len(m.segments)-1]
	if last.durMS > 0 {
		return last.startMS + last.durMS
	}
	// Previous segment's duration is unknown: the current anchor is the best
	// evidence for where it ended.
	if m.timelineMS > last.startMS {
		m.segments[len(m.segments)-1].durMS = m.timelineMS - last.startMS
		return m.timelineMS
	}
	return last.startMS
}

// selectSegment picks the RPL segment for a timeline position, using the Playout ID
// only to break a tie between overlapping segments (rule 4). Returns -1 when no RPL
// is held.
func (m *Machine) selectSegment(posMS int64, playoutID string) int {
	if len(m.segments) == 0 {
		return -1
	}
	first := -1
	for i, s := range m.segments {
		if !s.contains(posMS) {
			continue
		}
		if playoutID != "" && s.playout == playoutID {
			return i // overlapping segments: the ID breaks the tie
		}
		if first < 0 {
			first = i
		}
	}
	if first >= 0 {
		return first
	}
	// Nothing covers the position (a gap, or a timeline past everything we hold):
	// the last segment that starts at or before it.
	best := -1
	for i, s := range m.segments {
		if s.startMS <= posMS {
			best = i
		}
	}
	if best >= 0 {
		return best
	}
	return 0
}
