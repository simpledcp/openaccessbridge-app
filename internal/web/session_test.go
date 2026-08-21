package web

import (
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

func newSessions(t *testing.T, auditoriumID string) *SessionStore {
	t.Helper()
	st, err := NewSessionStore(auditoriumID, nil)
	if err != nil {
		t.Fatalf("NewSessionStore: %v", err)
	}
	return st
}

func TestSessionTokenRoundTrip(t *testing.T) {
	st := newSessions(t, "screen-3")
	issued, err := st.Issue(IssueOptions{ShowID: "show-a"})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Token == "" || issued.JoinCode == "" {
		t.Fatalf("issue produced no onboarding artifacts: %+v", issued)
	}
	if len(issued.JoinCode) != joinCodeLen {
		t.Fatalf("join code %q is not %d characters", issued.JoinCode, joinCodeLen)
	}
	got, err := st.Validate(issued.Token)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.ShowID != "show-a" || got.AuditoriumID != "screen-3" {
		t.Fatalf("claims round-tripped wrong: %+v", got)
	}
	for _, m := range []model.PatronMode{model.ModeCaption, model.ModeHI, model.ModeVIN} {
		if !got.Grants(m) {
			t.Fatalf("default session does not grant %s", m)
		}
	}
}

// §9.4: sessions auto-expire. Expiry travels inside the token, so it survives a
// restart that loses the in-memory store.
func TestSessionExpires(t *testing.T) {
	st := newSessions(t, "screen-3")
	base := time.Now()
	st.now = func() time.Time { return base }
	issued, err := st.Issue(IssueOptions{ShowID: "s", Lifetime: 30 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Validate(issued.Token); err != nil {
		t.Fatalf("fresh token rejected: %v", err)
	}
	st.now = func() time.Time { return base.Add(31 * time.Minute) }
	if _, err := st.Validate(issued.Token); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expired token = %v want ErrSessionExpired", err)
	}
}

func TestSessionLifetimeIsCapped(t *testing.T) {
	st := newSessions(t, "screen-3")
	issued, err := st.Issue(IssueOptions{ShowID: "s", Lifetime: 72 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if got := issued.ExpiresAt.Sub(issued.IssuedAt); got > MaxSessionLifetime {
		t.Fatalf("lifetime %v exceeds the %v cap", got, MaxSessionLifetime)
	}
}

// A well-signed token with no expiry must be refused outright: §9.4's guarantee is
// that a per-show grant ends, and an unbounded one silently never does.
func TestTokenWithoutExpiryIsRefused(t *testing.T) {
	st := newSessions(t, "screen-3")
	payload := []byte("sid|show|screen-3|caption|0")
	tok := tokenVersion + "." +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(st.mac(payload))
	if _, err := st.Validate(tok); !errors.Is(err, ErrNoExpiry) {
		t.Fatalf("expiry-less token = %v want ErrNoExpiry", err)
	}
}

// §8.5: "yesterday's code cannot silently admit patrons to today's wrong room."
func TestTokenFromAnotherAuditoriumIsRefused(t *testing.T) {
	screen3 := newSessions(t, "screen-3")
	issued, err := screen3.Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// Same appliance image, same signing key, different room.
	screen4 := newSessions(t, "screen-4")
	screen4.key = screen3.key
	if _, err := screen4.Validate(issued.Token); !errors.Is(err, ErrWrongAuditorium) {
		t.Fatalf("cross-room token = %v want ErrWrongAuditorium", err)
	}
}

func TestForgedSignatureIsRefused(t *testing.T) {
	st := newSessions(t, "screen-3")
	issued, err := st.Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(issued.Token, ".")
	forged := parts[0] + "." + parts[1] + "." + base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdef"))
	if _, err := st.Validate(forged); !errors.Is(err, ErrBadToken) {
		t.Fatalf("forged token = %v want ErrBadToken", err)
	}
}

// §9.4 "no carry-over between shows": ending a show revokes its tokens and tells
// live patron connections, so the page can show "this show has ended".
func TestEndShowRevokesAndSignals(t *testing.T) {
	st := newSessions(t, "screen-3")
	issued, err := st.Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ended := st.ShowEnded()
	select {
	case <-ended:
		t.Fatalf("show reported ended before it ended")
	default:
	}
	st.EndShow()
	select {
	case <-ended:
	default:
		t.Fatalf("EndShow did not signal live patron connections")
	}
	if _, err := st.Validate(issued.Token); !errors.Is(err, ErrShowEnded) {
		t.Fatalf("post-show token = %v want ErrShowEnded", err)
	}
	if _, ok := st.Current(); ok {
		t.Fatalf("a session survived the show end")
	}
}

func TestIssuingSupersedesThePreviousShow(t *testing.T) {
	st := newSessions(t, "screen-3")
	first, err := st.Issue(IssueOptions{ShowID: "show-1"})
	if err != nil {
		t.Fatal(err)
	}
	watching := st.ShowEnded()
	second, err := st.Issue(IssueOptions{ShowID: "show-2"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-watching:
	default:
		t.Fatalf("starting a new show did not end the previous one for connected patrons")
	}
	if _, err := st.Validate(first.Token); !errors.Is(err, ErrShowEnded) {
		t.Fatalf("previous show's token = %v want ErrShowEnded", err)
	}
	if _, err := st.Validate(second.Token); err != nil {
		t.Fatalf("new show's token rejected: %v", err)
	}
}

// The first session after boot must not tell an idle page that a show just ended.
func TestFirstIssueDoesNotSignalShowEnd(t *testing.T) {
	st := newSessions(t, "screen-3")
	watching := st.ShowEnded()
	if _, err := st.Issue(IssueOptions{ShowID: "show-1"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-watching:
		t.Fatalf("the first issued session reported a show end")
	default:
	}
}

func TestJoinCodeIsForgiving(t *testing.T) {
	st := newSessions(t, "screen-3")
	issued, err := st.Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	typed := strings.ToLower(issued.JoinCode[:4] + "-" + issued.JoinCode[4:])
	got, err := st.ExchangeCode(typed)
	if err != nil {
		t.Fatalf("ExchangeCode(%q): %v", typed, err)
	}
	if got.Token != issued.Token {
		t.Fatalf("join code returned a different session")
	}
	if _, err := st.ExchangeCode("00000000"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("unknown code = %v want ErrBadToken", err)
	}
}

// A mid-show restart must not lock out patrons holding a valid code, which is why
// the signing key is configuration rather than process state.
func TestSigningKeySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := config.OpenSQLite(filepath.Join(dir, "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	first, err := NewSessionStore("screen-3", store)
	if err != nil {
		t.Fatal(err)
	}
	issued, err := first.Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	// A fresh process: new in-memory store, same config store.
	second, err := NewSessionStore("screen-3", store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Validate(issued.Token); err != nil {
		t.Fatalf("token issued before a restart was rejected after it: %v", err)
	}
	// Sessions themselves are transient and must NOT be persisted (§9.4).
	if _, ok := second.Current(); ok {
		t.Fatalf("a session survived a restart — sessions must be memory-only")
	}
}

// --- the gate on the patron surface ---

func newTokenServer(t *testing.T, perShow bool) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile:      model.ProfilePatronFacing,
		PatronBind:   "0.0.0.0:0",
		AdminBind:    "127.0.0.1:0",
		Auditorium:   "Auditorium 3",
		AuditoriumID: "screen-3",
		PerShowToken: perShow,
	}, Deps{Hub: fanout.NewHub(fanout.Options{})})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// §9.4: production profiles require a per-show token. Before this change the
// model existed and nothing validated anything.
func TestPatronSurfaceRequiresAPerShowToken(t *testing.T) {
	s := newTokenServer(t, true)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/caption/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("untokened caption stream = %d want 401", resp.StatusCode)
	}

	// The page itself stays usable: it asks for the code instead of erroring out.
	page := getBody(t, ts.URL+"/accessibility")
	if !strings.Contains(page, `id="joincode"`) {
		t.Fatalf("patron page does not offer the join-code path")
	}
	if strings.Contains(page, "token") || strings.Contains(page, "401") {
		t.Fatalf("patron page leaks protocol detail: %q", firstLines(page, 40))
	}
}

func TestScannedTokenBecomesACookieAndIsAccepted(t *testing.T) {
	s := newTokenServer(t, true)
	issued, err := s.Sessions().Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	jar := &recordingJar{}
	cli := &http.Client{Jar: jar}
	resp, err := cli.Get(ts.URL + "/accessibility?t=" + issued.Token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scanned join = %d", resp.StatusCode)
	}
	if !jar.sawSessionCookie {
		t.Fatalf("scanning the code did not establish a session cookie")
	}
	if resp.Request.URL.RawQuery != "" {
		t.Fatalf("token was left in the address bar: %q", resp.Request.URL.String())
	}
	// The follow-up API call carries only the cookie.
	r2, err := cli.Get(ts.URL + "/api/audio/streams")
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusOK {
		t.Fatalf("cookie-authorized audio call = %d want 200", r2.StatusCode)
	}
}

// Lab and Booth Diagnostic (deploy.Profile.PerShowToken=false) stay open.
func TestProfileWithoutPerShowTokenStaysOpen(t *testing.T) {
	s := newTokenServer(t, false)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/api/audio/streams")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open profile audio call = %d want 200", resp.StatusCode)
	}
}

// recordingJar is a minimal cookie jar that also records whether the session
// cookie was ever set.
type recordingJar struct {
	cookies          []*http.Cookie
	sawSessionCookie bool
}

func (j *recordingJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	for _, c := range cookies {
		if c.Name == sessionCookie && c.Value != "" {
			j.sawSessionCookie = true
		}
	}
	j.cookies = cookies
}

func (j *recordingJar) Cookies(u *url.URL) []*http.Cookie { return j.cookies }
