package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/model"
)

// D8's priority order, plus the BCP-47 fallback the patron path never had.
func TestNegotiateLanguage(t *testing.T) {
	avail := []string{"en", "es-419"}
	cases := []struct {
		name        string
		explicit    string
		accept      []string
		venue       string
		available   []string
		wantServing string
		wantSub     bool
	}{
		{"en-US falls back to the en track", "en-US", nil, "en", avail, "en", false},
		{"en-GB falls back to the en track", "en-GB", nil, "en", avail, "en", false},
		{"es-MX resolves to the regional es track", "es-MX", nil, "en", avail, "es-419", false},
		{"exact match wins", "es-419", nil, "en", avail, "es-419", false},
		{"case-insensitive exact match", "ES-419", nil, "en", avail, "es-419", false},
		{"an unavailable explicit choice substitutes LOUDLY", "de", nil, "en", avail, "en", true},
		{"venue default is preferred over the browser", "", []string{"es-419"}, "en", avail, "en", false},
		{"browser match applies when the venue default is absent", "", []string{"es-MX"}, "fr", avail, "es-419", false},
		{"first available when nothing matches", "", []string{"fr"}, "fr", avail, "en", false},
		{"no tracks loaded: honour the request verbatim", "en-US", nil, "en", nil, "en-US", false},
		{"no tracks and no request: browser tag", "", []string{"es-MX"}, "en", nil, "es-MX", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			serving, _, sub := negotiateLanguage(c.explicit, c.accept, c.venue, c.available)
			if serving != c.wantServing || sub != c.wantSub {
				t.Fatalf("negotiateLanguage(%q,%v,%q,%v) = (%q,%v) want (%q,%v)",
					c.explicit, c.accept, c.venue, c.available, serving, sub, c.wantServing, c.wantSub)
			}
		})
	}
}

func TestParseAcceptLanguageOrdersByQ(t *testing.T) {
	got := parseAcceptLanguage("fr;q=0.5, en-US, es-419;q=0.8, *;q=0.1")
	want := []string{"en-US", "es-419", "fr"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseAcceptLanguage = %v want %v", got, want)
	}
	if got := parseAcceptLanguage("  "); got != nil {
		t.Fatalf("empty header = %v want nil", got)
	}
	if got := parseAcceptLanguage("de;q=0"); len(got) != 0 {
		t.Fatalf("q=0 must be dropped, got %v", got)
	}
}

// The reported failure, end to end: a phone reporting en-US used to receive one
// {"visible":false,...,"status":"active"} frame and then nothing — a blank page
// with a green "Connected" badge for the whole film.
func TestPhoneReportingEnUSReceivesTheEnglishCues(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{Auditorium: "Auditorium 3"})
	s := newLangServer(t, hub)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/caption/stream?lang=en-US", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	time.Sleep(50 * time.Millisecond)
	hub.PublishCue("en", true, model.Cue{Text: "Welcome to CaptionBridge.", ResourceID: "r1", StartMS: 1000})
	if !scanFor(resp.Body, "Welcome to CaptionBridge.", 2*time.Second) {
		t.Fatalf("en-US subscriber never received the en cues")
	}
}

// A patron who explicitly picks a language the show does not carry must be told,
// not silently served another one (guardrail #7).
func TestUnavailableLanguageSubstitutionIsAnnounced(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{})
	s := newLangServer(t, hub)
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/caption/stream?lang=de", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !scanFor(resp.Body, `"substituted":true`, 2*time.Second) {
		t.Fatalf("a substituted language was not announced to the patron")
	}
}

// Accept-Language had zero readers anywhere in the repo, so D8 step (2) could
// never fire on the patron path.
func TestAcceptLanguageDrivesTheDefaultWhenTheVenueDefaultIsAbsent(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{})
	s, err := NewServer(Config{
		Profile: model.ProfileLab, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
		Auditorium: "Auditorium 3", DefaultLanguage: "fr", // not carried by this show
	}, Deps{Hub: hub, Languages: func() []LangOption {
		return []LangOption{{Code: "en", Label: "en"}, {Code: "es-419", Label: "es-419"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/api/caption/stream", nil)
	req.Header.Set("Accept-Language", "es-MX,es;q=0.9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if !scanFor(resp.Body, `"serving":"es-419"`, 2*time.Second) {
		t.Fatalf("Accept-Language did not select the es-419 track")
	}
}

func newLangServer(t *testing.T, hub *fanout.Hub) *Server {
	t.Helper()
	s, err := NewServer(Config{
		Profile: model.ProfileLab, PatronBind: "0.0.0.0:0", AdminBind: "127.0.0.1:0",
		Auditorium: "Auditorium 3", DefaultLanguage: "en",
	}, Deps{Hub: hub, Languages: func() []LangOption {
		return []LangOption{{Code: "en", Label: "en"}, {Code: "es-419", Label: "es-419"}}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
