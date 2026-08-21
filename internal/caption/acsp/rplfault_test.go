package acsp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// A playlist the appliance cannot load used to produce ONE LOG LINE: no state
// change, no warning, nothing on the dashboard. Guardrail #7 is not audio-only,
// and this is the caption plane's version of a silent degradation.
func TestAnUnloadablePlaylistRaisesAWarning(t *testing.T) {
	var raised []model.Warning
	m := New(Options{
		OnWarning: func(w model.Warning) { raised = append(raised, w) },
		Resolver:  failingResolver{},
	})
	m.Connected()
	resp := m.Handle(Request{Type: ReqSetRPLLocation, RPLLocation: "http://server/rpl.xml"})

	if resp.OK {
		t.Fatal("an unfetchable playlist reported success")
	}
	if len(raised) == 0 {
		t.Fatal("an unfetchable playlist raised no warning: the booth gets one log line and nothing else")
	}
	w := raised[len(raised)-1]
	if w.Code != model.WarnCaptionRPLUnavailable {
		t.Fatalf("warning code %q", w.Code)
	}
	if w.Severity != model.SeverityCritical {
		t.Fatalf("severity %q for a show that cannot caption", w.Severity)
	}
}

// A playlist that arrives but cannot be PARSED is a different fault with a
// different remedy — a content problem in the DCP, not a connection problem —
// and the message says so.
func TestAnUnparseablePlaylistSaysItIsThePlaylistNotTheLink(t *testing.T) {
	var raised []model.Warning
	m := New(Options{OnWarning: func(w model.Warning) { raised = append(raised, w) }})
	m.Connected()
	resp := m.Handle(Request{Type: ReqSetRPLLocation, RPLInline: []byte("<not-an-rpl")})

	if resp.OK {
		t.Fatal("an unparseable playlist reported success")
	}
	if len(raised) == 0 {
		t.Fatal("an unparseable playlist raised no warning")
	}
	if msg := raised[len(raised)-1].Message; !strings.Contains(msg, "not in the connection") {
		t.Fatalf("the message does not distinguish a content fault from a link fault: %q", msg)
	}
}

// THE PATRON-FACING HALF. After a failed Set RPL the server can still drive the
// transport to playing, and the machine advanced to PLAYING without ever
// checking that anything was loaded — reporting status "active" over a blank
// area. From a seat that is indistinguishable from a phone that has stopped.
func TestPlayingWithNoPlaylistIsNotReportedAsActive(t *testing.T) {
	state := model.CaptionLinkState{
		State:      model.StatePlaying,
		OutputMode: model.OutputEnabled,
		RPLLoaded:  false,
	}
	if got := state.PatronStatus(); got == model.PatronCaptionActive {
		t.Fatal("PLAYING with no playlist loaded is reported to the patron as working captions")
	}

	// And with a playlist it is active, so the check above is not simply
	// refusing everything.
	state.RPLLoaded = true
	if got := state.PatronStatus(); got != model.PatronCaptionActive {
		t.Fatalf("PLAYING with a playlist reports %q", got)
	}
}

// failingResolver stands in for a media block that cannot produce the playlist
// the server named — a locator form this appliance does not handle, a fetch that
// times out, a file that is not there.
type failingResolver struct{}

func (failingResolver) Resolve(context.Context, string) ([]byte, error) {
	return nil, errors.New("the playlist could not be fetched")
}
