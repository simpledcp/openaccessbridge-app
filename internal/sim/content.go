package sim

import (
	"fmt"
	"strings"

	"github.com/simpledcp/captionbridge/internal/caption/acsp"
)

// CueSpec is a synthetic cue (reel-relative ms).
type CueSpec struct {
	StartMS int64
	EndMS   int64
	Text    string
}

// ResSpec is a synthetic RPL resource.
type ResSpec struct {
	ID       string
	Type     string // ST 429-12 track type, e.g. "ClosedCaption"
	Language string
	Locator  string
}

// ReelSpec is a synthetic reel.
type ReelSpec struct {
	Number     int
	DurationMS int64 // reel duration on the composition timeline
	// EntryPointMS is where the reel starts inside its assets (0 for untrimmed).
	EntryPointMS int64
	Resources    []ResSpec
	// OmitReelTiming emits a reel with no reel-level <Duration>/<EntryPoint>, the
	// way content from a server that does not declare them arrives — the duration
	// then falls back to the longest resource (§5.3).
	OmitReelTiming bool
}

// TimedText builds a SMPTE-428-style timed-text document where the editable-unit
// rate is 1000 (so units == milliseconds), keeping known-answer math trivial.
func TimedText(language string, cues []CueSpec) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<SubtitleReel xmlns="urn:sim:428">`)
	fmt.Fprintf(&sb, `<Language>%s</Language>`, language)
	sb.WriteString(`<EditRate>1000 1</EditRate><TimeCodeRate>1000</TimeCodeRate>`)
	sb.WriteString(`<SubtitleList><Font>`)
	for _, c := range cues {
		fmt.Fprintf(&sb, `<Subtitle TimeIn="%s" TimeOut="%s"><Text>%s</Text></Subtitle>`,
			tc(c.StartMS), tc(c.EndMS), xmlEscape(c.Text))
	}
	sb.WriteString(`</Font></SubtitleList></SubtitleReel>`)
	return []byte(sb.String())
}

// RPL builds a synthetic ST 430-11 RPL where resource durations are in
// millisecond editable units (EditRate 1000), so reel offsets equal cumulative
// reel durations in ms.
func RPL(playoutID string, reels []ReelSpec) []byte {
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	sb.WriteString(`<AuxResourcePresentationList xmlns="urn:sim:430-11">`)
	sb.WriteString(`<Id>urn:uuid:00000000-0000-0000-0000-000000000000</Id>`)
	fmt.Fprintf(&sb, `<PlayoutId>%s</PlayoutId>`, playoutID)
	sb.WriteString(`<ReelList>`)
	for _, r := range reels {
		sb.WriteString(`<Reel>`)
		fmt.Fprintf(&sb, `<ReelNumber>%d</ReelNumber>`, r.Number)
		// Reel-level timing. An uncaptioned reel legitimately has no resources at
		// all (§5.2 item 5), so this is the only thing that keeps the reels after
		// it anchored on the composition timeline (§5.3).
		if !r.OmitReelTiming {
			sb.WriteString(`<EditRate>1000 1</EditRate>`)
			fmt.Fprintf(&sb, `<EntryPoint>%d</EntryPoint>`, r.EntryPointMS)
			fmt.Fprintf(&sb, `<Duration>%d</Duration>`, r.DurationMS)
		}
		sb.WriteString(`<ResourceList>`)
		for _, res := range r.Resources {
			sb.WriteString(`<Resource>`)
			fmt.Fprintf(&sb, `<Id>%s</Id>`, res.ID)
			fmt.Fprintf(&sb, `<Type>%s</Type>`, res.Type)
			fmt.Fprintf(&sb, `<Language>%s</Language>`, res.Language)
			sb.WriteString(`<EditRate>1000 1</EditRate><EntryPoint>0</EntryPoint>`)
			fmt.Fprintf(&sb, `<IntrinsicDuration>%d</IntrinsicDuration>`, r.DurationMS)
			fmt.Fprintf(&sb, `<Locator>%s</Locator>`, res.Locator)
			sb.WriteString(`</Resource>`)
		}
		sb.WriteString(`</ResourceList></Reel>`)
	}
	sb.WriteString(`</ReelList></AuxResourcePresentationList>`)
	return []byte(sb.String())
}

func tc(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	totalSec := ms / 1000
	u := ms % 1000
	h := totalSec / 3600
	m := (totalSec % 3600) / 60
	s := totalSec % 60
	return fmt.Sprintf("%02d:%02d:%02d:%03d", h, m, s, u)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

// --- Canonical server request-sequence helpers (§5.2) ---

// Announce builds an Announce request.
func Announce(serverID, screenName, fingerprint string) acsp.Request {
	return acsp.Request{Type: acsp.ReqAnnounce, ServerID: serverID, ScreenName: screenName, Fingerprint: fingerprint}
}

// GetNewLease builds a Get New Lease request.
func GetNewLease(leaseMS int64) acsp.Request {
	return acsp.Request{Type: acsp.ReqGetNewLease, LeaseMS: leaseMS}
}

// UpdateTimeline builds an Update Timeline request.
func UpdateTimeline(timelineMS int64, playoutID string, state acsp.PlayState) acsp.Request {
	return acsp.Request{Type: acsp.ReqUpdateTimeline, TimelineMS: timelineMS, PlayoutID: playoutID, PlayState: state}
}

// SetRPLInline builds a Set RPL Location request carrying the RPL inline.
func SetRPLInline(rpl []byte) acsp.Request {
	return acsp.Request{Type: acsp.ReqSetRPLLocation, RPLInline: rpl}
}

// SetRPLLocation builds a Set RPL Location request referencing a media-block locator.
func SetRPLLocation(locator string) acsp.Request {
	return acsp.Request{Type: acsp.ReqSetRPLLocation, RPLLocation: locator}
}

// SetOutputMode builds a Set Output Mode request (enabled/disabled).
func SetOutputMode(enabled bool) acsp.Request {
	return acsp.Request{Type: acsp.ReqSetOutputMode, OutputEnabled: acsp.BoolPtr(enabled)}
}

// Status builds a periodic status keepalive request (half-lease cadence).
func Status(leaseMS int64) acsp.Request {
	return acsp.Request{Type: acsp.ReqStatus, LeaseMS: leaseMS}
}

// StatusAt builds a periodic status that also carries timeline state, which the
// §5.2 request list says the status request does. Servers that push the timeline
// only on status must still keep the caption link anchored.
func StatusAt(leaseMS, timelineMS int64, playoutID string, state acsp.PlayState) acsp.Request {
	return acsp.Request{Type: acsp.ReqStatus, LeaseMS: leaseMS,
		TimelineMS: timelineMS, PlayoutID: playoutID, PlayState: state}
}

// TerminateLease builds a Terminate Lease request (new-show boundary).
func TerminateLease() acsp.Request { return acsp.Request{Type: acsp.ReqTerminateLease} }
