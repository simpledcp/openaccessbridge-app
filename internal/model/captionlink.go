package model

// ACSPState is the ST 430-10 caption-link state (§5.2). It is modeled as a
// lease-and-timeline state machine, not a parser.
type ACSPState string

const (
	StateDisconnected   ACSPState = "DISCONNECTED"
	StateConnected      ACSPState = "CONNECTED"
	StateAnnounced      ACSPState = "ANNOUNCED"
	StateLeased         ACSPState = "LEASED"
	StateTimelineActive ACSPState = "TIMELINE_ACTIVE"
	StateRPLReceived    ACSPState = "RPL_RECEIVED"
	StateReady          ACSPState = "READY"
	StatePlaying        ACSPState = "PLAYING"
	StatePaused         ACSPState = "PAUSED"
	StateStopped        ACSPState = "STOPPED"
	StateShowChange     ACSPState = "SHOW_CHANGE"
	StateLeaseExpired   ACSPState = "LEASE_EXPIRED"
	StateError          ACSPState = "ERROR"
)

// OutputMode mirrors the server's Set Output Mode play/stop status (§5.2 item 3).
// Absence must be handled gracefully — assume Unknown rather than crash.
type OutputMode string

const (
	OutputEnabled  OutputMode = "enabled"
	OutputDisabled OutputMode = "disabled"
	OutputUnknown  OutputMode = "unknown"
)

// PatronCaptionStatus is the derived patron-facing caption state (§5.2 final
// paragraph). It deliberately hides protocol detail from patrons.
type PatronCaptionStatus string

const (
	PatronCaptionActive       PatronCaptionStatus = "active"
	PatronCaptionWaiting      PatronCaptionStatus = "waiting_for_show"
	PatronCaptionReconnecting PatronCaptionStatus = "temporarily_reconnecting"
	PatronCaptionNoCaptions   PatronCaptionStatus = "no_captions_for_content"
	PatronCaptionUnavailable  PatronCaptionStatus = "unavailable"
)

// CaptionLinkState is the observable ACSP link state (§10.3). It carries no
// caption text — only state, counts, timers, and opaque IDs.
type CaptionLinkState struct {
	State  ACSPState `json:"state"`
	Server string    `json:"server"`
	// ServerFamily is the cinema server family in force for this link: what the
	// server declared on announce, else the auditorium profile's configured
	// value. EMPTY means not known — the wire field is PROVISIONAL (BLOCKER B1)
	// and many servers will declare nothing. It is the attribution key for
	// §5.3's unresolved-locator observations, so an "unknown" placeholder would
	// merge two vendors into one misleading bucket.
	ServerFamily       string     `json:"server_family,omitempty"`
	LeaseActive        bool       `json:"lease_active"`
	LeaseExpiresInMS   int64      `json:"lease_expires_in_ms"`
	RequestIntervalMS  int64      `json:"request_interval_ms"` // == half lease (§5.2 item 1)
	TimelinePositionMS int64      `json:"timeline_position_ms"`
	OutputMode         OutputMode `json:"output_mode"`
	CurrentPlayoutID   string     `json:"current_playout_id"`
	RPLLoaded          bool       `json:"rpl_loaded"`
	AssetsLoaded       bool       `json:"assets_loaded"`
	CurrentLanguage    string     `json:"current_language"`
	// AvailableLanguages is every caption language the current show carries, in
	// D8 priority order (venue preference first, then most-accessible track kind).
	// Consumers may treat the first entry as the language to serve by default.
	AvailableLanguages []string `json:"available_languages"`
	// LastTimelineAnchorMonotonicNS is the monotonic timestamp of the last
	// timeline anchor (§5.5); used by the timing engine, surfaced for diagnostics.
	LastTimelineAnchorMonotonicNS int64 `json:"last_timeline_anchor_monotonic_ns"`
	// DriftMSEstimate is the measured drift exposed to diagnostics (§5.5).
	DriftMSEstimate int64 `json:"drift_ms_estimate"`
}

// PatronStatus maps the internal link state to the patron-facing status.
// This is the single place protocol state becomes a calm patron message; it must
// never present an unavailable/degraded service as working (guardrail #7).
func (s CaptionLinkState) PatronStatus() PatronCaptionStatus {
	switch s.State {
	case StatePlaying:
		if s.OutputMode == OutputDisabled {
			return PatronCaptionNoCaptions
		}
		// PLAYING with no playlist loaded is not a working caption service.
		//
		// It is reachable: a Set RPL Location that could not be fetched or parsed
		// leaves the machine in its previous state, and the next Update Timeline
		// with PlayPlaying advances to PLAYING without checking that anything was
		// ever loaded. The result was status "active" with no cue — a patron
		// staring at a blank area being told, in effect, that captions are
		// working. That is the silent degradation guardrail #7 forbids, and it is
		// the shape a deaf patron cannot distinguish from a broken phone.
		if !s.RPLLoaded {
			return PatronCaptionUnavailable
		}
		return PatronCaptionActive
	case StateReady, StateRPLReceived, StateTimelineActive, StateLeased, StateAnnounced, StateConnected:
		return PatronCaptionWaiting
	case StatePaused, StateStopped, StateShowChange:
		return PatronCaptionWaiting
	case StateLeaseExpired:
		// Lease genuinely expired: only now do we blank/declare unavailable
		// (§5.2 item 2 — comms loss alone keeps showing captions).
		return PatronCaptionUnavailable
	case StateDisconnected, StateError:
		return PatronCaptionUnavailable
	default:
		return PatronCaptionUnavailable
	}
}

// CaptionReady reports the top-line caption readiness verdict (§10.6). Captions
// are ready when the link is playing with output enabled (or could be) and the
// assets/RPL are loaded.
func (s CaptionLinkState) CaptionReady() bool {
	switch s.State {
	case StatePlaying, StateReady:
		return s.LeaseActive && s.RPLLoaded
	default:
		return false
	}
}

// ServerConnected reports §8.2's "Server connected: yes/no" — whether an ACSP
// link to the cinema server is up at all.
//
// DISCONNECTED and ERROR both answer no: ERROR is a fault the link cannot carry
// on from, and rendering a faulted link as connected is exactly the silent
// degradation guardrail #7 forbids.
func (s CaptionLinkState) ServerConnected() bool {
	switch s.State {
	case StateDisconnected, StateError, "":
		return false
	default:
		return true
	}
}

// TimelineState is §8.2's "Timeline: playing / paused / stopped" row — the
// transport state the cinema server last drove the caption path with.
//
// It is deliberately NOT derived from ACSPState. ACSPState answers "where is the
// lease-and-timeline handshake up to", which is a different question: a link can
// sit in TIMELINE_ACTIVE having been told a position but never a play state. A
// row that answered "stopped" for "nobody has said" would be inventing a
// transport state the server never sent (guardrail #7).
type TimelineState string

const (
	// TimelineNone: no play state is being driven — the show has not started, the
	// lease died, or the composition changed and the next anchor has not landed.
	TimelineNone    TimelineState = "no_timeline"
	TimelinePlaying TimelineState = "playing"
	TimelinePaused  TimelineState = "paused"
	TimelineStopped TimelineState = "stopped"
)

// Label renders the timeline row for a booth tech.
func (t TimelineState) Label() string {
	switch t {
	case TimelinePlaying:
		return "playing"
	case TimelinePaused:
		return "paused"
	case TimelineStopped:
		return "stopped"
	default:
		return "no timeline"
	}
}
