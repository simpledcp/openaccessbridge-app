package model

import "time"

// PatronMode is a grantable patron capability.
type PatronMode string

const (
	ModeCaption PatronMode = "caption"
	ModeHI      PatronMode = "HI"
	ModeVIN     PatronMode = "VI-N"
)

// PatronSession is a per-show access grant (§10.4). Sessions auto-expire at show
// end; no carry-over between shows. Tokens are per-show and carry no personal
// data (§10.7 patron privacy).
type PatronSession struct {
	SessionID          string       `json:"session_id"`
	ShowID             string       `json:"show_id"` // opaque per-show; not a title
	Token              string       `json:"token"`   // signed or coded
	AuditoriumID       string       `json:"auditorium_id"`
	GrantedModes       []PatronMode `json:"granted_modes"`
	IssuedAt           time.Time    `json:"issued_at"`
	ExpiresAt          time.Time    `json:"expires_at"` // show end
	ClientCountCaption int          `json:"client_count_caption"`
	ClientCountAudio   int          `json:"client_count_audio"`
}

// Expired reports whether the session is past its expiry at time now.
func (s PatronSession) Expired(now time.Time) bool {
	return !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt)
}

// Grants reports whether the session grants the given mode.
func (s PatronSession) Grants(m PatronMode) bool {
	for _, g := range s.GrantedModes {
		if g == m {
			return true
		}
	}
	return false
}

// ParsePatronMode parses a grantable mode name, reporting whether it is known.
// An unknown mode is REJECTED rather than dropped: a token that silently lost a
// grant would present a degraded experience as a working one (guardrail #7).
func ParsePatronMode(s string) (PatronMode, bool) {
	switch PatronMode(s) {
	case ModeCaption:
		return ModeCaption, true
	case ModeHI:
		return ModeHI, true
	case ModeVIN:
		return ModeVIN, true
	default:
		return "", false
	}
}

// Valid reports whether m is a known grantable mode.
func (m PatronMode) Valid() bool {
	_, ok := ParsePatronMode(string(m))
	return ok
}

// AudioStream maps a granted audio mode to its named stream (§6.5 / §10.2).
// ModeCaption has no audio stream and reports false.
func (m PatronMode) AudioStream() (StreamType, bool) {
	switch m {
	case ModeHI:
		return StreamHI, true
	case ModeVIN:
		return StreamVIN, true
	default:
		return "", false
	}
}

// ModeForStream is the inverse of AudioStream.
func ModeForStream(s StreamType) (PatronMode, bool) {
	switch s {
	case StreamHI:
		return ModeHI, true
	case StreamVIN:
		return ModeVIN, true
	default:
		return "", false
	}
}

// RemainingAt reports how long the session still grants access at time now.
// A session with no expiry reports zero: §9.4 requires per-show sessions that
// auto-expire at show end, so an unbounded grant is a defect, not a convenience.
func (s PatronSession) RemainingAt(now time.Time) time.Duration {
	if s.ExpiresAt.IsZero() {
		return 0
	}
	if d := s.ExpiresAt.Sub(now); d > 0 {
		return d
	}
	return 0
}
