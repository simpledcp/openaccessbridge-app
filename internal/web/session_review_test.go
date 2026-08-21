package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Revocation used to be keyed by SHOW id, which is operator-supplied and repeats:
// re-issuing a code for the show already running (a lost door card, a second
// screening of the same title) marked that show ended and then minted a token
// carrying the very id that had just been revoked. The admin call still answered
// 200 with a code, so the failure only surfaced at the door (guardrail #7).
func TestReissuingTheSameShowIDMintsAUsableSession(t *testing.T) {
	st := newSessions(t, "screen-3")
	if _, err := st.Issue(IssueOptions{ShowID: "matinee-1"}); err != nil {
		t.Fatal(err)
	}
	second, err := st.Issue(IssueOptions{ShowID: "matinee-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Validate(second.Token); err != nil {
		t.Fatalf("re-issued token for the same show = %v, want a usable session", err)
	}
	if _, ok := st.Current(); !ok {
		t.Fatalf("Current() reports no active session immediately after Issue")
	}
	if _, err := st.ExchangeCode(second.JoinCode); err != nil {
		t.Fatalf("join code for the re-issued session = %v", err)
	}
}

// Superseding still has to revoke the session it replaced, even when the two
// carry the same show id.
func TestReissueStillRevokesTheSupersededSession(t *testing.T) {
	st := newSessions(t, "screen-3")
	first, err := st.Issue(IssueOptions{ShowID: "matinee-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Issue(IssueOptions{ShowID: "matinee-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Validate(first.Token); !errors.Is(err, ErrShowEnded) {
		t.Fatalf("superseded token = %v want ErrShowEnded", err)
	}
}

// §10.4: the patron page must render only the modes the session actually grants.
// Drawing a caption box whose transport answers 403 leaves the patron on
// "Reconnecting…" for the whole film with nothing explaining why.
func TestPageRendersOnlyTheGrantedModes(t *testing.T) {
	cases := []struct {
		name     string
		modes    []model.PatronMode
		path     string
		wantHave []string
		wantNot  []string
	}{
		{
			name:  "audio-only code drops the caption controls",
			modes: []model.PatronMode{model.ModeHI}, path: "/accessibility",
			wantHave: []string{`id="streampicker"`, "Captions are not included"},
			wantNot:  []string{`id="caption"`, `id="lang"`},
		},
		{
			name:  "caption-only code drops the listening panel",
			modes: []model.PatronMode{model.ModeCaption}, path: "/accessibility",
			wantHave: []string{`id="caption"`, "Assistive audio is not included"},
			wantNot:  []string{`id="streampicker"`, `id="volume"`, `id="buffer"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newTokenServer(t, true)
			issued, err := s.Sessions().Issue(IssueOptions{ShowID: "s", Modes: c.modes})
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(s.PatronHandler())
			defer ts.Close()
			body := scanBody(t, ts.URL+c.path+"?t="+issued.Token)
			for _, w := range c.wantHave {
				if !strings.Contains(body, w) {
					t.Errorf("page is missing %q", w)
				}
			}
			for _, w := range c.wantNot {
				if strings.Contains(body, w) {
					t.Errorf("page draws %s that this session cannot use", w)
				}
			}
		})
	}
}

// §7.2 show-end behavior applies to a RELOAD too: a patron whose show has ended
// gets the polite state, not a form inviting them to type a code that is dead.
func TestPageAfterShowEndIsPoliteAndOffersNoDeadForm(t *testing.T) {
	s := newTokenServer(t, true)
	issued, err := s.Sessions().Issue(IssueOptions{ShowID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	s.EndShow()
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()
	body := scanBody(t, ts.URL+"/accessibility?t="+issued.Token)
	if !strings.Contains(body, "This show has ended") {
		t.Fatalf("page after show end does not carry the polite copy: %q", firstLines(body, 30))
	}
	if strings.Contains(body, `id="joincode"`) {
		t.Fatalf("page after show end still offers a join form")
	}
}

// scanBody fetches a page the way a phone does: the scanned token is exchanged
// for a cookie on the redirect, so the client needs a jar.
func scanBody(t *testing.T, url string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	cli := &http.Client{Jar: jar}
	resp, err := cli.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// §10.4's client_count_caption was hardcoded to zero even though the fan-out hub
// that knows the answer is already injected.
func TestSessionReportsLiveCaptionClientCount(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{})
	s, err := NewServer(Config{
		Profile: model.ProfileLab, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
		Auditorium: "Auditorium 3",
	}, Deps{Hub: hub})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := hub.Subscribe("en")
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Unsubscribe(sub)

	ts := httptest.NewServer(s.AdminHandler())
	defer ts.Close()
	resp, err := http.Post(ts.URL+"/api/admin/session", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out SessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.ClientCountCaption != 1 {
		t.Fatalf("client_count_caption = %d want 1 live caption subscriber", out.ClientCountCaption)
	}
}

// A template failure must not leave a half-drawn page behind a 200. From a seat
// in the dark a truncated page is indistinguishable from a broken appliance, and
// the old path wrote the template straight to the ResponseWriter and only then
// tried to send a status.
func TestPatronPageRenderFailureIsWholeNotPartial(t *testing.T) {
	s := newTestServer(t, model.ProfileLab, fanout.NewHub(fanout.Options{}))
	s.patronTmpl = template.Must(template.New("boom").Parse(
		`<html><body>PARTIAL{{call .Boom}}</body></html>`))

	rec := httptest.NewRecorder()
	s.renderPatron(rec, patronPageData{Mode: "caption"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("render failure = %d want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "PARTIAL") {
		t.Fatalf("a half-rendered page reached the patron: %q", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "template") || strings.Contains(rec.Body.String(), "Boom") {
		t.Fatalf("render failure leaked a diagnostic to the patron: %q", rec.Body.String())
	}
}

// §9.4's gate must fail CLOSED. active_profile is a free-form string in the
// config store and deploy.Get's zero Profile has PerShowToken=false, so reading
// it without checking the ok opened the patron surface of a locked install on a
// typo or a renamed profile.
func TestUnknownProfileStillRequiresAPerShowToken(t *testing.T) {
	cases := []struct {
		profile model.FirewallProfile
		want    bool
	}{
		{model.ProfileLab, false},
		{model.ProfileBoothDiagnostic, false},
		{model.ProfilePatronFacing, true},
		{model.ProfileLowCostReplacement, true},
		{model.FirewallProfile("patron-facing"), true}, // a plausible typo
		{model.FirewallProfile(""), true},
	}
	for _, c := range cases {
		if got := RequirePerShowToken(c.profile); got != c.want {
			t.Errorf("RequirePerShowToken(%q) = %v want %v", c.profile, got, c.want)
		}
	}
}
