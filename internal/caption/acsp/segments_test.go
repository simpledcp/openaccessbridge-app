package acsp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/rpl"
)

// segRPL builds an RPL of `reels` equal-length reels at 1000 units/s (units == ms),
// with resource IDs/locators namespaced by name so two RPLs can share a Playout ID.
func segRPL(playout, name string, reels int, reelMS int64) []byte {
	var sb strings.Builder
	sb.WriteString(`<AuxResourcePresentationList xmlns="x">`)
	fmt.Fprintf(&sb, `<PlayoutId>%s</PlayoutId><ReelList>`, playout)
	for i := 1; i <= reels; i++ {
		fmt.Fprintf(&sb, `<Reel><ReelNumber>%d</ReelNumber>`, i)
		fmt.Fprintf(&sb, `<EditRate>1000 1</EditRate><EntryPoint>0</EntryPoint><Duration>%d</Duration>`, reelMS)
		fmt.Fprintf(&sb, `<ResourceList><Resource><Id>%s-r%d</Id><Type>ClosedCaption</Type>`, name, i)
		fmt.Fprintf(&sb, `<Language>en</Language><EditRate>1000 1</EditRate><EntryPoint>0</EntryPoint>`)
		fmt.Fprintf(&sb, `<IntrinsicDuration>%d</IntrinsicDuration><Locator>%s_r%d.xml</Locator>`, reelMS, name, i)
		sb.WriteString(`</Resource></ResourceList></Reel>`)
	}
	sb.WriteString(`</ReelList></AuxResourcePresentationList>`)
	return []byte(sb.String())
}

func mustParse(t *testing.T, b []byte) *rpl.RPL {
	t.Helper()
	r, err := rpl.ParseBytes(b)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	return r
}

// put parses and stores an RPL the way handleSetRPL does.
func (m *Machine) put(t *testing.T, b []byte) int {
	t.Helper()
	return m.putSegment(mustParse(t, b), fingerprint(b))
}

// TestSegmentPlacement covers where an incoming RPL lands on the composition
// timeline: from zero, chained after the previous segment, or — on a mid-show
// reconnect where the server sends only the segment now playing — at the anchor.
func TestSegmentPlacement(t *testing.T) {
	t.Run("first RPL presents the composition from zero", func(t *testing.T) {
		m := &Machine{activeSeg: -1, timelineMS: 25000}
		i := m.put(t, segRPL("1", "showa", 3, 60000))
		if got := m.segments[i].startMS; got != 0 {
			t.Errorf("start = %d, want 0", got)
		}
	})
	t.Run("reconnect past the RPL's whole duration anchors at the timeline", func(t *testing.T) {
		m := &Machine{activeSeg: -1, timelineMS: 125000}
		i := m.put(t, segRPL("2", "feature", 1, 60000))
		if got := m.segments[i].startMS; got != 125000 {
			t.Errorf("start = %d, want 125000 (only the playing segment was sent)", got)
		}
	})
	t.Run("a second RPL chains after the first", func(t *testing.T) {
		m := &Machine{activeSeg: -1}
		m.put(t, segRPL("0", "trailer", 1, 60000))
		i := m.put(t, segRPL("1", "feature", 2, 60000))
		if got := m.segments[i].startMS; got != 60000 {
			t.Errorf("start = %d, want 60000 (after the trailer)", got)
		}
		if got := m.segments[i].endMS(); got != 180000 {
			t.Errorf("end = %d, want 180000", got)
		}
	})
	t.Run("an unknown-duration segment is closed by the next RPL", func(t *testing.T) {
		m := &Machine{activeSeg: -1}
		// No reel durations at all: the segment's length is unknown.
		m.put(t, []byte(`<AuxResourcePresentationList xmlns="x"><PlayoutId>0</PlayoutId>`+
			`<ReelList><Reel><ReelNumber>1</ReelNumber><ResourceList/></Reel></ReelList>`+
			`</AuxResourcePresentationList>`))
		if got := m.segments[0].endMS(); got <= 0 {
			t.Fatalf("unknown duration should be open-ended, got end %d", got)
		}
		m.timelineMS = 42000
		i := m.put(t, segRPL("1", "feature", 1, 60000))
		if got := m.segments[i].startMS; got != 42000 {
			t.Errorf("start = %d, want 42000", got)
		}
		if got := m.segments[0].endMS(); got != 42000 {
			t.Errorf("previous segment end = %d, want it closed at 42000", got)
		}
	})
}

// TestSegmentResendReplacesInPlace: a server re-sending the same RPL must not push
// a duplicate segment down the timeline, but two different compositions sharing a
// Playout ID must both be kept (§5.2 item 4).
func TestSegmentResendReplacesInPlace(t *testing.T) {
	m := &Machine{activeSeg: -1}
	doc := segRPL("7", "showa", 1, 60000)
	m.put(t, doc)
	m.put(t, doc) // byte-identical re-send
	if got := len(m.segments); got != 1 {
		t.Fatalf("segments after a re-send = %d, want 1", got)
	}
	// Same RPL, different serialization (whitespace): still a re-send.
	spaced := []byte(strings.Replace(string(doc), "<ReelList>", "<ReelList> ", 1))
	m.put(t, spaced)
	if got := len(m.segments); got != 1 {
		t.Fatalf("segments after a re-serialized re-send = %d, want 1", got)
	}
	// A DIFFERENT composition that happens to share the Playout ID is a new segment.
	m.put(t, segRPL("7", "showb", 1, 60000))
	if got := len(m.segments); got != 2 {
		t.Fatalf("segments after a different composition = %d, want 2", got)
	}
}

// TestSegmentRetentionIsBounded: a server that never sends Terminate Lease must not
// be able to grow the retained set without limit (§5.2 new-show-load).
func TestSegmentRetentionIsBounded(t *testing.T) {
	m := &Machine{activeSeg: -1}
	for i := 0; i < maxRetainedSegments+4; i++ {
		idx := m.put(t, segRPL(fmt.Sprint(i), fmt.Sprintf("show%d", i), 1, 60000))
		if idx < 0 || idx >= len(m.segments) {
			t.Fatalf("putSegment returned out-of-range index %d (len %d)", idx, len(m.segments))
		}
	}
	if got := len(m.segments); got != maxRetainedSegments {
		t.Fatalf("retained segments = %d, want the cap %d", got, maxRetainedSegments)
	}
}

// TestSelectSegmentByPosition: position decides, the Playout ID only breaks ties.
func TestSelectSegmentByPosition(t *testing.T) {
	m := &Machine{activeSeg: -1}
	m.put(t, segRPL("0", "trailer", 1, 60000))  // [0, 60000)
	m.put(t, segRPL("1", "feature", 2, 60000))  // [60000, 180000)
	m.put(t, segRPL("0", "postroll", 1, 30000)) // [180000, 210000), id 0 again

	cases := []struct {
		name    string
		pos     int64
		playout string
		want    int
	}{
		{"inside the trailer", 1000, "0", 0},
		{"inside the feature", 61000, "1", 1},
		{"a stale zero id during the feature does not win", 61000, "0", 1},
		{"an empty id still resolves by position", 125000, "", 1},
		{"the second segment sharing id 0", 185000, "0", 2},
		{"past everything falls back to the last segment", 999000, "", 2},
		{"before everything falls back to the first", -1000, "", 0},
	}
	for _, tc := range cases {
		if got := m.selectSegment(tc.pos, tc.playout); got != tc.want {
			t.Errorf("%s: selectSegment(%d, %q) = %d, want %d", tc.name, tc.pos, tc.playout, got, tc.want)
		}
	}
	if got := (&Machine{activeSeg: -1}).selectSegment(0, "1"); got != -1 {
		t.Errorf("selectSegment with no RPL = %d, want -1", got)
	}
}
