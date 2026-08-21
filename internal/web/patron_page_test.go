package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// §7.1 defines three routes as three EXPERIENCES. Before this change all three
// served one byte-identical captions-only document, so /listen had no audio and
// /caption shipped controls it must not have.
func TestPatronRoutesAreThreeDifferentExperiences(t *testing.T) {
	s := newTestServer(t, model.ProfileLab, fanout.NewHub(fanout.Options{}))
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	cases := []struct {
		path     string
		mode     string
		wantHave []string
		wantNot  []string
	}{
		{
			path: "/caption", mode: "caption",
			wantHave: []string{`id="caption"`, `id="lang"`},
			wantNot:  []string{`id="streampicker"`, `id="buffer"`, `id="volume"`},
		},
		{
			path: "/listen", mode: "listen",
			wantHave: []string{`id="streampicker"`, `id="buffer"`, `id="volume"`},
			wantNot:  []string{`id="caption"`, `id="lang"`},
		},
		{
			path: "/accessibility", mode: "accessibility",
			wantHave: []string{`id="caption"`, `id="lang"`, `id="streampicker"`, `id="buffer"`},
		},
	}
	for _, c := range cases {
		body := getBody(t, ts.URL+c.path)
		if !strings.Contains(body, `data-mode="`+c.mode+`"`) {
			t.Errorf("%s: page does not declare mode %q", c.path, c.mode)
		}
		for _, w := range c.wantHave {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %s", c.path, w)
			}
		}
		for _, w := range c.wantNot {
			if strings.Contains(body, w) {
				t.Errorf("%s: must not contain %s", c.path, w)
			}
		}
	}
}

// §7.2: "Auditorium identity, always visible". It must be right before any
// connection is made, not only once the fan-out pushes a state.
func TestPatronPageShowsRoomIdentityBeforeConnecting(t *testing.T) {
	s := newTestServer(t, model.ProfileLab, fanout.NewHub(fanout.Options{}))
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()
	body := getBody(t, ts.URL+"/accessibility")
	if !strings.Contains(body, "Auditorium 3") {
		t.Fatalf("patron page does not carry the room identity: %q", firstLines(body, 20))
	}
}

// §7.2: "No admin/diagnostic leakage." The patron document must not name a bind
// address, an interface, a device, a profile or a server.
func TestPatronPageLeaksNoDiagnostics(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{})
	s, err := NewServer(Config{
		Profile:    model.ProfileBoothDiagnostic,
		PatronBind: "192.168.44.1:8080",
		AdminBind:  "127.0.0.1:8090",
		Auditorium: "Auditorium 3",
	}, Deps{
		Hub: hub,
		Audio: func() []model.AudioStreamState {
			return []model.AudioStreamState{{
				StreamType: model.StreamHI, InputDeviceID: "usb-Focusrite-Scarlett-00",
				Status: model.QualFailed, Reasons: []string{"input appears mono, but this profile requires two channels"},
			}}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.PatronHandler())
	defer ts.Close()

	forbidden := []string{"192.168.44.1", "127.0.0.1", "8090", "booth_diagnostic", "usb-Focusrite", "requires two channels", "eth0", "wlan0"}
	for _, path := range []string{"/accessibility", "/caption", "/listen", "/api/audio/streams", "/api/caption/languages"} {
		body := getBody(t, ts.URL+path)
		for _, f := range forbidden {
			if strings.Contains(body, f) {
				t.Errorf("%s leaks %q to the patron surface", path, f)
			}
		}
	}
}

func getBody(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
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

func firstLines(s string, n int) string {
	parts := strings.SplitN(s, "\n", n+1)
	if len(parts) > n {
		parts = parts[:n]
	}
	return strings.Join(parts, "\n")
}
