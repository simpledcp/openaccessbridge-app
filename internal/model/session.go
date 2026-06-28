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
