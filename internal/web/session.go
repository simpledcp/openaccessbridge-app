package web

// Per-show patron sessions and one-scan onboarding (buildplan.md §7.2, §9.4, §10.4).
//
// §7.2: "a per-show QR encodes the local URL + session token (Section 10.4) so
// patrons join in one scan". §9.4: "Per-show access code or signed QR token for
// patron access; sessions auto-expire at show end." §8.5: "Per-show QR codes
// expire and include the auditorium identifier, so yesterday's code cannot
// silently admit patrons to today's wrong room."
//
// WHERE THE SIGNING KEY LIVES. Tokens are HMAC-SHA256 over a compact claim
// string, and the key is a 32-byte random secret held in the CONFIG store as a
// setting. The alternative — a key generated fresh at every process start — was
// rejected: a mid-show restart of openaccessbridge-core would invalidate every
// displayed code at once and the patrons holding them would have no staff-free way
// back in. Persisting the key is safe here because it is configuration, not
// content (guardrail #5 covers caption/audio content), it sits beside the admin
// password hashes in the same store, and it is NOT what bounds a session's life:
//
//   - every token MUST carry an expiry (an expiry-less token is refused) and the
//     issuer caps it at MaxSessionLifetime, so "yesterday's code" dies on time
//     regardless of the key;
//   - every token carries the auditorium id and is refused by any other room
//     (§8.5 wrong-room safeguard);
//   - ending a show revokes its tokens immediately, in memory.
//
// Sessions themselves are TRANSIENT: this store is in-memory only and is never
// written to the config store (§9.4 no-persistence). A restart therefore loses
// revocations; the expiry carried inside the signed token is what survives, which
// is exactly why the expiry is mandatory rather than optional.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/deploy"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// settingPatronSessionKey names the config setting holding the token signing key.
// It is declared here rather than in internal/config because it is owned by the
// patron surface; the value is a hex-encoded random secret, never a typed
// credential.
const settingPatronSessionKey = "patron_session_signing_key"

// Session lifetime policy (§9.4 "auto-expire at show end"). A show is bounded, so
// the issuer refuses to mint anything longer than MaxSessionLifetime.
const (
	// DefaultSessionLifetime covers trailers, a feature, and a late start.
	DefaultSessionLifetime = 4 * time.Hour
	// MaxSessionLifetime is the hard cap: no token may outlive a single show day.
	MaxSessionLifetime = 12 * time.Hour
)

// tokenVersion prefixes every token so the format can change unambiguously.
const tokenVersion = "cb1"

// joinCodeAlphabet is Crockford-style base32 minus I, L, O and U: unambiguous when
// read aloud in a dark auditorium or printed on a door card.
const joinCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// joinCodeLen is the length of the staff/patron-typed short code.
const joinCodeLen = 8

// Session errors. Each maps to calm patron copy in patron.go; the codes
// themselves are never shown to a patron (§7.2 no diagnostic leakage).
var (
	ErrNoSession       = errors.New("no session token presented")
	ErrBadToken        = errors.New("session token is not valid for this appliance")
	ErrSessionExpired  = errors.New("session has expired")
	ErrWrongAuditorium = errors.New("session token was issued for a different auditorium")
	ErrShowEnded       = errors.New("the show this session was issued for has ended")
	ErrNoExpiry        = errors.New("session token carries no expiry")
)

// RequirePerShowToken reports whether a deployment profile requires a per-show
// patron token on the patron surface (§9.4; deploy.Profile.PerShowToken is true
// for Patron-Facing and Low-Cost Replacement, false for Lab and Booth
// Diagnostic).
//
// An UNKNOWN profile requires one. active_profile is a free-form string in the
// config store, and deploy.Get's zero Profile has PerShowToken=false — so
// reading it without checking the ok would silently OPEN the patron surface of a
// locked install on a typo or a renamed profile. A security gate fails closed.
func RequirePerShowToken(profile model.FirewallProfile) bool {
	p, ok := deploy.Get(profile)
	if !ok {
		return true
	}
	return p.PerShowToken
}

// SessionStore mints and validates per-show patron sessions. It is in-memory only
// (§9.4); the signing key is the sole thing it reads from or writes to config.
type SessionStore struct {
	auditoriumID string
	key          []byte
	now          func() time.Time

	mu sync.Mutex
	// byCode maps a short join code to its live session (staff-assisted entry).
	byCode map[string]model.PatronSession
	// codeBySession is the reverse lookup so the admin surface can re-read a code.
	codeBySession map[string]string
	// endedSessions records revoked sessions by SESSION id, with the expiry they
	// carry so the map can be pruned. Keying this by SHOW id was wrong: a show id
	// is operator-supplied and repeats (the same title, or a re-issued code for
	// the show already running), and a repeat then minted a token that was
	// revoked before it was ever handed out. Exactly one session is live at a
	// time — Issue supersedes — so the session id revokes the same grant without
	// poisoning a future one.
	endedSessions map[string]time.Time
	// showEnd is closed when the current show ends, so live patron connections can
	// deliver a polite "this show has ended" instead of dangling (§7.2).
	showEnd chan struct{}
	// current is the most recently issued session, for the admin surface.
	current *model.PatronSession
	// audioClients counts live audio signaling connections (§10.4 client_count_*).
	audioClients int
}

// NewSessionStore builds a store bound to one auditorium id. If store is non-nil
// the signing key is loaded (or created) there; otherwise an ephemeral key is
// generated, which is right for tests and for a Lab bench where no token is
// required anyway.
func NewSessionStore(auditoriumID string, store config.Store) (*SessionStore, error) {
	key, err := loadOrCreateSigningKey(store)
	if err != nil {
		return nil, err
	}
	return &SessionStore{
		auditoriumID:  auditoriumID,
		key:           key,
		now:           time.Now,
		byCode:        make(map[string]model.PatronSession),
		codeBySession: make(map[string]string),
		endedSessions: make(map[string]time.Time),
		showEnd:       make(chan struct{}),
	}, nil
}

func loadOrCreateSigningKey(store config.Store) ([]byte, error) {
	if store != nil {
		if v, ok, err := store.GetSetting(settingPatronSessionKey); err == nil && ok && v != "" {
			if b, derr := hex.DecodeString(v); derr == nil && len(b) == 32 {
				return b, nil
			}
			// A corrupt key is not silently tolerated: fall through and mint a new
			// one, which invalidates outstanding tokens loudly rather than
			// accepting signatures we cannot reproduce.
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate patron session signing key: %w", err)
	}
	if store != nil {
		if err := store.PutSetting(settingPatronSessionKey, hex.EncodeToString(key)); err != nil {
			return nil, fmt.Errorf("persist patron session signing key: %w", err)
		}
	}
	return key, nil
}

// IssueOptions describes a session to mint.
type IssueOptions struct {
	// ShowID is opaque and per-show. It must never be a title (§10.4, §9.4).
	ShowID string
	// Modes are the granted capabilities; empty grants all three.
	Modes []model.PatronMode
	// Lifetime is capped at MaxSessionLifetime; zero uses DefaultSessionLifetime.
	Lifetime time.Duration
}

// IssuedSession is a minted session plus the artifacts an operator needs to get
// patrons in: the short code and the payload a QR would carry.
type IssuedSession struct {
	model.PatronSession
	// JoinCode is the short, human-typed alternative to scanning (§9.4 "access
	// code OR signed QR token").
	JoinCode string `json:"join_code"`
}

// Issue mints a new per-show session. Issuing supersedes any previous show: its
// tokens are revoked and its patrons are told the show ended (§9.4 no carry-over).
func (s *SessionStore) Issue(opts IssueOptions) (IssuedSession, error) {
	modes := opts.Modes
	if len(modes) == 0 {
		modes = []model.PatronMode{model.ModeCaption, model.ModeHI, model.ModeVIN}
	}
	for _, m := range modes {
		if !m.Valid() {
			return IssuedSession{}, fmt.Errorf("unknown patron mode %q", string(m))
		}
	}
	life := opts.Lifetime
	if life <= 0 {
		life = DefaultSessionLifetime
	}
	if life > MaxSessionLifetime {
		life = MaxSessionLifetime
	}
	showID := strings.TrimSpace(opts.ShowID)
	if showID == "" {
		showID = randomID(6)
	}
	// The claim string is pipe-delimited, so a pipe in an operator-supplied field
	// would make it ambiguous. Refuse rather than mangle.
	if strings.ContainsAny(showID, "|") || strings.ContainsAny(s.auditoriumID, "|") {
		return IssuedSession{}, errors.New(`show id and auditorium id must not contain "|"`)
	}

	now := s.now()
	sess := model.PatronSession{
		SessionID:    randomID(8),
		ShowID:       showID,
		AuditoriumID: s.auditoriumID,
		GrantedModes: modes,
		IssuedAt:     now.UTC(),
		ExpiresAt:    now.Add(life).UTC(),
	}
	sess.Token = s.sign(sess)
	code := s.newJoinCode()

	s.mu.Lock()
	// Only signal show-end if there genuinely was a previous show; the first issue
	// after boot must not tell an idle page that a show just ended.
	s.endShowLocked(s.current != nil)
	s.byCode[code] = sess
	s.codeBySession[sess.SessionID] = code
	cur := sess
	s.current = &cur
	s.mu.Unlock()

	return IssuedSession{PatronSession: sess, JoinCode: code}, nil
}

// JoinCode returns the short code for a session id, if it is still live.
func (s *SessionStore) JoinCode(sessionID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codeBySession[sessionID]
	return c, ok
}

// Current returns the most recently issued session if it is still valid.
func (s *SessionStore) Current() (IssuedSession, bool) {
	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()
	if cur == nil {
		return IssuedSession{}, false
	}
	if _, err := s.Validate(cur.Token); err != nil {
		return IssuedSession{}, false
	}
	out := *cur
	out.ClientCountAudio = s.AudioClients()
	code, _ := s.JoinCode(out.SessionID)
	return IssuedSession{PatronSession: out, JoinCode: code}, true
}

// ExchangeCode swaps a short join code for its session (staff-assisted entry for a
// patron who cannot scan). Codes are in-memory only and do not survive a restart;
// the signed token in the QR does.
func (s *SessionStore) ExchangeCode(code string) (model.PatronSession, error) {
	code = normalizeJoinCode(code)
	if code == "" {
		return model.PatronSession{}, ErrNoSession
	}
	s.mu.Lock()
	sess, ok := s.byCode[code]
	s.mu.Unlock()
	if !ok {
		return model.PatronSession{}, ErrBadToken
	}
	return s.Validate(sess.Token)
}

// Validate verifies a token's signature, expiry, auditorium binding and show
// revocation, returning the session it encodes.
func (s *SessionStore) Validate(token string) (model.PatronSession, error) {
	if strings.TrimSpace(token) == "" {
		return model.PatronSession{}, ErrNoSession
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != tokenVersion {
		return model.PatronSession{}, ErrBadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return model.PatronSession{}, ErrBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return model.PatronSession{}, ErrBadToken
	}
	if subtle.ConstantTimeCompare(sig, s.mac(payload)) != 1 {
		return model.PatronSession{}, ErrBadToken
	}
	sess, err := decodeClaims(string(payload))
	if err != nil {
		return model.PatronSession{}, err
	}
	sess.Token = token
	if sess.ExpiresAt.IsZero() {
		return model.PatronSession{}, ErrNoExpiry
	}
	// §8.5: the auditorium identifier is IN the code, and a code from another room
	// is refused rather than quietly admitted.
	if !strings.EqualFold(sess.AuditoriumID, s.auditoriumID) {
		return model.PatronSession{}, ErrWrongAuditorium
	}
	if sess.Expired(s.now()) {
		return model.PatronSession{}, ErrSessionExpired
	}
	s.mu.Lock()
	_, ended := s.endedSessions[sess.SessionID]
	s.mu.Unlock()
	if ended {
		return model.PatronSession{}, ErrShowEnded
	}
	return sess, nil
}

// EndShow revokes the current show's tokens and signals live patron connections so
// they can show "this show has ended" rather than dangling (§7.2, §9.4). It is
// safe to call when no show is active.
func (s *SessionStore) EndShow() {
	s.mu.Lock()
	s.endShowLocked(true)
	s.mu.Unlock()
}

func (s *SessionStore) endShowLocked(signal bool) {
	if s.current != nil {
		s.endedSessions[s.current.SessionID] = s.current.ExpiresAt
		s.current = nil
	}
	s.byCode = make(map[string]model.PatronSession)
	s.codeBySession = make(map[string]string)
	s.pruneEndedLocked()
	if signal {
		select {
		case <-s.showEnd: // already closed for this show
		default:
			close(s.showEnd)
		}
		s.showEnd = make(chan struct{})
	}
}

// pruneEndedLocked drops revocations that can no longer matter because the token
// they cover has expired on its own.
func (s *SessionStore) pruneEndedLocked() {
	now := s.now()
	for id, exp := range s.endedSessions {
		if !exp.IsZero() && now.After(exp) {
			delete(s.endedSessions, id)
		}
	}
}

// ShowEnded returns a channel closed when the current show ends. Callers capture
// it at connection start; a later show gets a fresh channel.
func (s *SessionStore) ShowEnded() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.showEnd
}

// AudioClients reports the number of live audio signaling connections (§10.4).
func (s *SessionStore) AudioClients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioClients
}

func (s *SessionStore) addAudioClient(delta int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audioClients += delta
	if s.audioClients < 0 {
		s.audioClients = 0
	}
	return s.audioClients
}

// --- token codec ---

// encodeClaims renders the signed claim string.
func encodeClaims(s model.PatronSession) string {
	modes := make([]string, 0, len(s.GrantedModes))
	for _, m := range s.GrantedModes {
		modes = append(modes, string(m))
	}
	return strings.Join([]string{
		s.SessionID,
		s.ShowID,
		s.AuditoriumID,
		strings.Join(modes, ","),
		strconv.FormatInt(s.ExpiresAt.Unix(), 10),
	}, "|")
}

func decodeClaims(p string) (model.PatronSession, error) {
	f := strings.Split(p, "|")
	if len(f) != 5 {
		return model.PatronSession{}, ErrBadToken
	}
	exp, err := strconv.ParseInt(f[4], 10, 64)
	if err != nil {
		return model.PatronSession{}, ErrBadToken
	}
	var modes []model.PatronMode
	for _, m := range strings.Split(f[3], ",") {
		if m == "" {
			continue
		}
		pm, ok := model.ParsePatronMode(m)
		if !ok {
			return model.PatronSession{}, ErrBadToken
		}
		modes = append(modes, pm)
	}
	out := model.PatronSession{
		SessionID:    f[0],
		ShowID:       f[1],
		AuditoriumID: f[2],
		GrantedModes: modes,
	}
	if exp > 0 {
		out.ExpiresAt = time.Unix(exp, 0).UTC()
	}
	// exp <= 0 leaves ExpiresAt zero, which Validate refuses (ErrNoExpiry): an
	// unbounded grant is never accepted, however well signed it is (§9.4).
	return out, nil
}

func (s *SessionStore) sign(sess model.PatronSession) string {
	payload := []byte(encodeClaims(sess))
	return tokenVersion + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(s.mac(payload))
}

// mac truncates HMAC-SHA256 to 128 bits: ample against forgery for a local-only
// per-show grant, and short enough to keep the join URL scannable.
func (s *SessionStore) mac(payload []byte) []byte {
	h := hmac.New(sha256.New, s.key)
	h.Write(payload)
	return h.Sum(nil)[:16]
}

// --- ids and codes ---

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// A crypto/rand failure must not degrade to a predictable id
		// (guardrail #7: no silent degradation).
		panic("openaccessbridge: crypto/rand unavailable: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
}

func (s *SessionStore) newJoinCode() string {
	b := make([]byte, joinCodeLen)
	if _, err := rand.Read(b); err != nil {
		panic("openaccessbridge: crypto/rand unavailable: " + err.Error())
	}
	out := make([]byte, joinCodeLen)
	for i, v := range b {
		out[i] = joinCodeAlphabet[int(v)%len(joinCodeAlphabet)]
	}
	return string(out)
}

// normalizeJoinCode upper-cases and strips separators so a patron typing
// "abcd-1234" in the dark still gets in.
func normalizeJoinCode(in string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(in)) {
		if strings.ContainsRune(joinCodeAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}
