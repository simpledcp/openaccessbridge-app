// Package fanout pushes active-cue changes to patron browsers (buildplan.md §5.6).
// It is transport-agnostic: the Hub manages subscribers and per-language current
// state; the web layer adapts it to WebSocket (primary) and SSE (fallback).
//
// Design points from §5.6:
//   - Payload is the minimal cue (text, language, cue id, visible/cleared) plus the
//     patron-facing status — never raw ST 430-10/-11 server traffic.
//   - Multiple simultaneous languages: each subscriber selects one; switching does
//     not require a reload (SetLanguage).
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
	"strconv"
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
	Type       string `json:"type"`             // always "state"
	Visible    bool   `json:"visible"`          // is a cue currently shown
	Text       string `json:"text,omitempty"`   // caption text (transient content)
	Language   string `json:"language,omitempty"`
	CueID      string `json:"cue_id,omitempty"`
	Status     string `json:"status"`           // model.PatronCaptionStatus
	Auditorium string `json:"auditorium,omitempty"`
	Seq        int64  `json:"seq"`
}

type cueState struct {
	visible bool
	text    string
	cueID   string
}

// Hub fans caption state out to subscribers, keyed by language.
type Hub struct {
	mu         sync.Mutex
	subs       map[*Subscriber]struct{}
	cueByLang  map[string]cueState
	status     string
	auditorium string
	seq        int64
	maxClients int
	onCount    func(int)
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
		status:     string(model.PatronCaptionWaiting),
		auditorium: opts.Auditorium,
		maxClients: opts.MaxClients,
		onCount:    opts.OnCount,
	}
}

// buildLocked merges the current cue for a language with the global status.
func (h *Hub) buildLocked(language string) Message {
	cs := h.cueByLang[language]
	return Message{
		Type:       "state",
		Visible:    cs.visible,
		Text:       cs.text,
		Language:   language,
		CueID:      cs.cueID,
		Status:     h.status,
		Auditorium: h.auditorium,
		Seq:        h.seq,
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
	msg := h.buildLocked(language)
	targets := h.subsForLocked(language)
	h.mu.Unlock()
	for _, s := range targets {
		s.deliver(msg)
	}
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
