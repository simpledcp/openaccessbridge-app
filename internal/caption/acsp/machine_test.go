package acsp

import (
	"context"
	"testing"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/caption"
	"github.com/simpledcp/openaccessbridge/internal/caption/fanout"
	"github.com/simpledcp/openaccessbridge/internal/caption/resolver"
)

// rplDoc/ttDoc build minimal RPL + timed-text with ms editable units (rate 1000).
func ttDoc(lang, text string) []byte {
	return []byte(`<?xml version="1.0"?><SubtitleReel xmlns="x"><Language>` + lang +
		`</Language><EditRate>1000 1</EditRate><TimeCodeRate>1000</TimeCodeRate><SubtitleList><Font>` +
		`<Subtitle TimeIn="00:00:00:000" TimeOut="00:01:00:000"><Text>` + text + `</Text></Subtitle>` +
		`</Font></SubtitleList></SubtitleReel>`)
}

func rplDoc() []byte {
	return []byte(`<?xml version="1.0"?><AuxResourcePresentationList xmlns="x"><PlayoutId>1</PlayoutId>` +
		`<ReelList><Reel><ReelNumber>1</ReelNumber><ResourceList><Resource>` +
		`<Id>r1</Id><Type>ClosedCaption</Type><Language>en</Language>` +
		`<EditRate>1000 1</EditRate><EntryPoint>0</EntryPoint><IntrinsicDuration>60000</IntrinsicDuration>` +
		`<Locator>tt_en.xml</Locator></Resource></ResourceList></Reel></ReelList></AuxResourcePresentationList>`)
}

// TestRunOverTransport exercises the production Run loop + ChanTransport: a
// server-side ServerLink drives the canonical sequence and the patron hub receives
// the caption. This covers the real-time path the standalone simulator/appliance
// uses, complementing the deterministic scenario suite.
func TestRunOverTransport(t *testing.T) {
	hub := fanout.NewHub(fanout.Options{})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	defer ctrl.Close()

	fetcher := &resolver.MemoryFetcher{Files: map[string][]byte{"tt_en.xml": ttDoc("en", "Live cue")}}
	res := resolver.NewPluggable(fetcher, nil, nil)
	loader := resolver.NewLoader(resolver.NewPipeline(res, resolver.NewCache(1<<20), nil), 0)

	m := New(Options{Controller: ctrl, Resolver: res, Loader: loader})

	acs, srv := NewChanPair(8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = m.Run(ctx, acs) }()

	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	for _, req := range []Request{
		{Type: ReqAnnounce, ServerID: "srv", ScreenName: "Room"},
		{Type: ReqGetNewLease, LeaseMS: 30000},
		{Type: ReqUpdateTimeline, TimelineMS: 1000, PlayoutID: "1", PlayState: PlayPlaying},
		{Type: ReqSetRPLLocation, RPLInline: rplDoc()},
		{Type: ReqSetOutputMode, OutputEnabled: BoolPtr(true)},
	} {
		if err := srv.Send(sctx, req); err != nil {
			t.Fatalf("send %s: %v", req.Type, err)
		}
		if _, err := srv.Recv(sctx); err != nil {
			t.Fatalf("recv ack for %s: %v", req.Type, err)
		}
	}

	sub, _ := hub.Subscribe("en")
	// The cue [0,60000) is active at timeline 1000; it should arrive promptly.
	deadline := time.After(2 * time.Second)
	for {
		nctx, ncancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		msg, err := sub.Next(nctx)
		ncancel()
		if err == nil && msg.Visible && msg.Text == "Live cue" {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("did not receive live caption over the transport path")
		default:
		}
	}
}
