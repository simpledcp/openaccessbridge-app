// Package fanout pushes active-cue changes to patron browsers (buildplan.md §5.6).
// It is transport-agnostic: the Hub manages subscribers and per-language current
// state; the web layer adapts it to WebSocket (primary) and SSE (fallback).
//
// Design points from §5.6:
//   - Payload is the minimal cue (text, language, cue id, visible/cleared) plus the
//     patron-facing status — never raw ST 430-10/-11 server traffic.
//   - Multiple simultaneous languages: each subscriber selects one; switching does
//     not require a reload (SetLanguage). The hub owns the set of served languages
//     and their track grades, so the §5.4 / D8 default-language priority lives
//     here too (NegotiateLanguage) rather than being re-derived per transport.
//   - Stateless reconnect: a (re)subscribing phone immediately receives the current
//     merged state, so it never waits for the next cue boundary.
//   - Backpressure: delivery coalesces to the LATEST state per subscriber and never
//     blocks the timing engine on a slow client. For captions, showing the latest
//     state is always correct, so coalescing (not queueing) is the right policy.
//
// Caption text is transient CONTENT delivered only to authorized local patrons; it
// is never logged (the logging layer redacts it) and never persisted.
package fanout

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/simpledcp/captionbridge/internal/model"
)

// ErrTooManyClients is returned when the per-profile client cap is reached (D11).
var ErrTooManyClients = errors.New("too many caption clients")

// ErrClosed is returned by Next when the subscriber has been closed.
var ErrClosed = errors.New("subscriber closed")

// Message is the full caption state snapshot sent to a patron. Every push is a
// complete state (cue + patron status), which fits coalesce-to-latest delivery and
// lets the patron UI render from a single message.
type Message struct {
	Type       string `json:"type"`           // always "state"
	Visible    bool   `json:"visible"`        // is a cue currently shown
	Text       string `json:"text,omitempty"` // caption text (transient content)
	Language   string `json:"language,omitempty"`
	CueID      string `json:"cue_id,omitempty"`
	Status     string `json:"status"` // model.PatronCaptionStatus
	Auditorium string `json:"auditorium,omitempty"`
	Seq        int64  `json:"seq"`
	// Kind is the ST 429-12 track kind serving this language and CaptionGrade
	// says whether it is caption-grade (speaker IDs, sound effects, music cues)
	// rather than subtitle-grade. The patron UI must say "Subtitles available"
	// rather than "Closed captions" when it is not, so a patron relying on
	// caption-grade information is not misled (§5.4).
	Kind         string `json:"kind,omitempty"`
	CaptionGrade bool   `json:"caption_grade"`
	// LanguageAvailable is false when the show carries no track for the
	// subscriber's selected language. The patron UI must say so — a blank
	// caption area forever is exactly the silent degradation guardrail #7
	// forbids.
	LanguageAvailable bool `json:"language_available"`
}

type cueState struct {
	visible bool
	text    string
	cueID   string
}

// Hub fans caption state out to subscribers, keyed by language.
type Hub struct {
	mu        sync.Mutex
	subs      map[*Subscriber]struct{}
	cueByLang map[string]cueState
	// kindByLang records which track kind serves each language. Presence in this
	// map is what makes a language "available": it is set when a reel loads, so
	// the patron gets the right label before the first cue.
	kindByLang map[string]model.TrackKind
	status     string
	auditorium string
	seq        int64
	maxClients int
	onCount    func(int)
}

// LanguageOption is one caption language the hub is serving, with the grade of
// the track behind it (§5.4).
type LanguageOption struct {
	Code string          `json:"code"`
	Kind model.TrackKind `json:"kind,omitempty"`
}

// Options configures a Hub.
type Options struct {
	// MaxClients caps concurrent subscribers (0 = unlimited). Per-profile (D11).
	MaxClients int
	// Auditorium is the room identity shown to patrons (§7.2 / §8.5).
	Auditorium string
	// OnCount is called with the live subscriber count on connect/disconnect, for
	// the dashboard "connected caption clients" tile (§8.2).
	OnCount func(int)
}

// NewHub builds a Hub.
func NewHub(opts Options) *Hub {
	return &Hub{
		subs:       make(map[*Subscriber]struct{}),
		cueByLang:  make(map[string]cueState),
		kindByLang: make(map[string]model.TrackKind),
		status:     string(model.PatronCaptionWaiting),
		auditorium: opts.Auditorium,
		maxClients: opts.MaxClients,
		onCount:    opts.OnCount,
	}
}

// MaxClients returns the configured per-profile client cap (0 = unlimited, D11).
func (h *Hub) MaxClients() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.maxClients
}

// buildLocked merges the current cue for a language with the global status.
func (h *Hub) buildLocked(language string) Message {
	cs := h.cueByLang[language]
	kind, known := h.kindByLang[language]
	return Message{
		Type:              "state",
		Visible:           cs.visible,
		Text:              cs.text,
		Language:          language,
		CueID:             cs.cueID,
		Status:            h.status,
		Auditorium:        h.auditorium,
		Seq:               h.seq,
		Kind:              string(kind),
		CaptionGrade:      kind.IsCaption(),
		LanguageAvailable: known,
	}
}

// PublishCue updates the current cue for a language and delivers the merged state
// to that language's subscribers. visible=false clears the cue.
func (h *Hub) PublishCue(language string, visible bool, cue model.Cue) {
	h.mu.Lock()
	h.seq++
	cs := cueState{visible: visible}
	if visible {
		cs.text = cue.Text
		cs.cueID = cue.ResourceID + "@" + strconv.FormatInt(cue.StartMS, 10)
	}
	h.cueByLang[language] = cs
	// A VISIBLE cue proves the language is being served; its ST 429-12 kind
	// carries the caption-vs-subtitle grade through to the patron copy (§5.4).
	// Kind is sticky: it must outlive the cue so the label is right between cues
	// too. A cue-OUT proves nothing and must never mark a language available —
	// otherwise a retired engine's in-flight boundary timer, firing after
	// ClearLanguage, silently re-advertises a language the show no longer
	// carries (guardrail #7).
	switch {
	case cue.Kind != "" && cue.Kind != model.TrackUnknown:
		h.kindByLang[language] = cue.Kind
	case visible:
		if _, known := h.kindByLang[language]; !known {
			h.kindByLang[language] = model.TrackUnknown
		}
	}
	msg := h.buildLocked(language)
	targets := h.subsForLocked(language)
	h.mu.Unlock()
	for _, s := range targets {
		s.deliver(msg)
	}
}

// SetTrackKind records which ST 429-12 track kind serves a language and marks the
// language available, so the patron UI can label it correctly from the moment the
// reel loads rather than after the first cue (§5.4).
func (h *Hub) SetTrackKind(language string, kind model.TrackKind) {
	h.mu.Lock()
	if prev, known := h.kindByLang[language]; known && prev == kind {
		h.mu.Unlock()
		return
	}
	h.seq++
	h.kindByLang[language] = kind
	msg := h.buildLocked(language)
	targets := h.subsForLocked(language)
	h.mu.Unlock()
	for _, s := range targets {
		s.deliver(msg)
	}
}

// ClearLanguage retires a language the current reel/composition does not carry:
// its cue is blanked and it is marked unavailable. Without this a subscriber
// keeps the previous reel's last cue on screen indefinitely — "no captions for
// this segment" must be visible, not silent (§5.2 item 5).
func (h *Hub) ClearLanguage(language string) {
	h.mu.Lock()
	_, hadCue := h.cueByLang[language]
	_, hadKind := h.kindByLang[language]
	if !hadCue && !hadKind {
		h.mu.Unlock()
		return
	}
	h.seq++
	delete(h.cueByLang, language)
	delete(h.kindByLang, language)
	msg := h.buildLocked(language)
	targets := h.subsForLocked(language)
	h.mu.Unlock()
	for _, s := range targets {
		s.deliver(msg)
	}
}

// CueVisible reports §8.2's "Current cue: visible / none" — whether ANY served
// language currently has a cue on screen, and whether that question can be
// answered at all (false, false = no language is loaded, so there is nothing to
// be visible or not).
//
// It returns VISIBILITY ONLY and deliberately exposes no way to reach the text:
// the admin dashboard is a diagnostic surface and §9.4's caption.cue.visible_no_text
// principle is that a cue's occurrence may be recorded, its words never. Caption
// text stays inside the hub, bound for authorized patrons alone.
func (h *Hub) CueVisible() (visible, known bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.kindByLang) == 0 {
		return false, false
	}
	for _, cs := range h.cueByLang {
		if cs.visible {
			return true, true
		}
	}
	return false, true
}

// HasLanguage reports whether the show currently carries a track for a language.
func (h *Hub) HasLanguage(language string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	_, ok := h.kindByLang[language]
	return ok
}

// Languages returns the caption languages currently served, ordered by
// accessibility rank and then by code. That order IS D8 steps 3 and 4 ("first
// ClosedCaption track; first subtitle track") — the hub has no RPL ordering of
// its own, and a deterministic order also keeps the patron's language list
// stable across reloads.
func (h *Hub) Languages() []LanguageOption {
	h.mu.Lock()
	out := make([]LanguageOption, 0, len(h.kindByLang))
	for code, kind := range h.kindByLang {
		out = append(out, LanguageOption{Code: code, Kind: kind})
	}
	h.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		ri, rj := out[i].Kind.AccessibilityRank(), out[j].Kind.AccessibilityRank()
		if ri != rj {
			return ri < rj
		}
		return out[i].Code < out[j].Code
	})
	return out
}

// NegotiateLanguage picks the caption language to serve a patron, applying the
// §5.4 / D8 default priority in order:
//
//  1. venue/show-configured default (configured, in the order given)
//  2. browser-language match (requested, in the order given): exact BCP-47 tag
//     first, then a primary-subtag match so en-GB lands on en-US
//  3. first ClosedCaption track
//  4. first subtitle track
//
// available must carry the hub's accessibility-ranked ordering (Hub.Languages),
// which is what makes steps 3 and 4 "the first one". It is transport-agnostic on
// purpose: the caller turns an Accept-Language header or a navigator.language
// into the requested list. ok is false only when the show carries no captions at
// all — the caller must then say so rather than subscribe to nothing.
func NegotiateLanguage(available []LanguageOption, configured, requested []string) (string, bool) {
	if len(available) == 0 {
		return "", false
	}
	for _, want := range configured {
		if code, ok := matchLanguage(available, want); ok {
			return code, true
		}
	}
	for _, want := range requested {
		if code, ok := matchLanguage(available, want); ok {
			return code, true
		}
	}
	return available[0].Code, true
}

// matchLanguage resolves one requested tag against the available set: exact
// (case-insensitive) tag, then primary subtag.
func matchLanguage(available []LanguageOption, want string) (string, bool) {
	want = strings.TrimSpace(want)
	if want == "" || want == "*" {
		return "", false
	}
	for _, o := range available {
		if strings.EqualFold(o.Code, want) {
			return o.Code, true
		}
	}
	base := primarySubtag(want)
	if base == "" {
		return "", false
	}
	for _, o := range available {
		if strings.EqualFold(primarySubtag(o.Code), base) {
			return o.Code, true
		}
	}
	return "", false
}

// primarySubtag returns the language part of a BCP-47 tag ("es-419" → "es").
func primarySubtag(tag string) string {
	if i := strings.IndexAny(tag, "-_"); i >= 0 {
		return tag[:i]
	}
	return tag
}

// PublishStatus updates the patron-facing status (e.g. waiting/reconnecting/active)
// and delivers each subscriber its language's merged state.
func (h *Hub) PublishStatus(status model.PatronCaptionStatus) {
	h.mu.Lock()
	h.seq++
	h.status = string(status)
	all := make([]*Subscriber, 0, len(h.subs))
	for s := range h.subs {
		all = append(all, s)
	}
	build := make(map[string]Message)
	h.mu.Unlock()

	for _, s := range all {
		lang := s.Language()
		h.mu.Lock()
		m, ok := build[lang]
		if !ok {
			m = h.buildLocked(lang)
			build[lang] = m
		}
		h.mu.Unlock()
		s.deliver(m)
	}
}

// SetAuditorium updates the room identity surfaced to patrons.
func (h *Hub) SetAuditorium(name string) {
	h.mu.Lock()
	h.auditorium = name
	h.mu.Unlock()
}

func (h *Hub) subsForLocked(language string) []*Subscriber {
	out := make([]*Subscriber, 0)
	for s := range h.subs {
		if s.Language() == language {
			out = append(out, s)
		}
	}
	return out
}

// Subscribe registers a subscriber for a language and immediately delivers the
// current merged state (stateless reconnect). Returns ErrTooManyClients at the cap.
func (h *Hub) Subscribe(language string) (*Subscriber, error) {
	h.mu.Lock()
	if h.maxClients > 0 && len(h.subs) >= h.maxClients {
		h.mu.Unlock()
		return nil, ErrTooManyClients
	}
	s := &Subscriber{hub: h, language: language, notify: make(chan struct{}, 1)}
	h.subs[s] = struct{}{}
	msg := h.buildLocked(language)
	n := len(h.subs)
	cb := h.onCount
	h.mu.Unlock()

	s.deliver(msg)
	if cb != nil {
		cb(n)
	}
	return s, nil
}

// Unsubscribe removes a subscriber.
func (h *Hub) Unsubscribe(s *Subscriber) {
	h.mu.Lock()
	if _, ok := h.subs[s]; !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, s)
	n := len(h.subs)
	cb := h.onCount
	h.mu.Unlock()
	s.close()
	if cb != nil {
		cb(n)
	}
}

// SetLanguage switches a subscriber's language without a reconnect and delivers
// the new language's current state.
func (h *Hub) SetLanguage(s *Subscriber, language string) {
	s.mu.Lock()
	s.language = language
	s.mu.Unlock()
	h.mu.Lock()
	msg := h.buildLocked(language)
	h.mu.Unlock()
	s.deliver(msg)
}

// ClientCount returns the number of connected subscribers.
func (h *Hub) ClientCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// Subscriber is a single patron connection. It coalesces to the latest state.
type Subscriber struct {
	hub      *Hub
	mu       sync.Mutex
	language string
	latest   Message
	has      bool
	notify   chan struct{}
	closed   bool
}

// Peek returns the most recently delivered state without consuming it. Useful for
// the simulator harness and diagnostics; delivery still coalesces to the latest.
func (s *Subscriber) Peek() Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

// Language returns the subscriber's selected language.
func (s *Subscriber) Language() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.language
}

func (s *Subscriber) deliver(m Message) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.latest = m
	s.has = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default: // already signaled; the latest value will be read
	}
}

func (s *Subscriber) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Next blocks until a new state is available (or ctx is done / the subscriber is
// closed), returning the latest coalesced state. A slow consumer never blocks the
// publisher; it simply observes the most recent state.
func (s *Subscriber) Next(ctx context.Context) (Message, error) {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return Message{}, ErrClosed
		}
		if s.has {
			m := s.latest
			s.has = false
			s.mu.Unlock()
			return m, nil
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-ctx.Done():
			return Message{}, ctx.Err()
		}
	}
}
