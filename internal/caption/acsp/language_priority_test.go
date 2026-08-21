package acsp

import (
	"testing"

	"github.com/simpledcp/captionbridge/internal/web"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/fanout"
	"github.com/simpledcp/captionbridge/internal/caption/resolver"
)

// mixedKindRPL carries two languages: a SUBTITLE track first in playlist order,
// then a CLOSED CAPTION track. This is the show that separates D8 step (3) from
// step (4) — a show where both exist and the playlist puts the wrong one first.
func mixedKindRPL() []byte {
	res := func(id, typ, lang, loc string) string {
		return `<Resource><Id>` + id + `</Id><Type>` + typ + `</Type><Language>` + lang +
			`</Language><EditRate>1000 1</EditRate><EntryPoint>0</EntryPoint>` +
			`<IntrinsicDuration>60000</IntrinsicDuration><Locator>` + loc + `</Locator></Resource>`
	}
	return []byte(`<?xml version="1.0"?><AuxResourcePresentationList xmlns="x"><PlayoutId>1</PlayoutId>` +
		`<ReelList><Reel><ReelNumber>1</ReelNumber><ResourceList>` +
		res("r-fr", "MainSubtitle", "fr", "tt_fr.xml") +
		res("r-en", "ClosedCaption", "en", "tt_en.xml") +
		`</ResourceList></Reel></ReelList></AuxResourcePresentationList>`)
}

func mixedKindMachine(t *testing.T, opts Options) *Machine {
	t.Helper()
	hub := fanout.NewHub(fanout.Options{})
	ctrl := caption.NewController(caption.Options{Hub: hub})
	ctrl.Start()
	t.Cleanup(ctrl.Close)
	fetcher := &resolver.MemoryFetcher{Files: map[string][]byte{
		"tt_fr.xml": ttDoc("fr", "Sous-titre"),
		"tt_en.xml": ttDoc("en", "[door slams] Get out."),
	}}
	res := resolver.NewPluggable(fetcher, nil, nil)
	opts.Controller = ctrl
	opts.Resolver = res
	opts.Loader = resolver.NewLoader(resolver.NewPipeline(res, resolver.NewCache(1<<20), nil), 0)
	m := New(opts)
	m.Handle(Request{Type: ReqAnnounce, ServerID: "srv"})
	m.Handle(Request{Type: ReqGetNewLease, LeaseMS: 30000})
	m.Handle(Request{Type: ReqUpdateTimeline, TimelineMS: 1000, PlayoutID: "1", PlayState: PlayPlaying})
	if resp := m.Handle(Request{Type: ReqSetRPLLocation, RPLInline: mixedKindRPL()}); !resp.OK {
		t.Fatalf("Set RPL Location = %+v", resp)
	}
	return m
}

// TestAClosedCaptionTrackOutranksASubtitleTrack is D8 steps (3) and (4), and it
// is the reason those two steps are written as two steps.
//
// A closed-caption track carries speaker attribution, sound effects and music
// cues. A subtitle track carries dialogue translation and nothing else. For a
// deaf or hard-of-hearing patron those are not two flavours of the same thing:
// the subtitle track omits precisely the information they came for. On a show
// carrying both, defaulting to whichever the playlist happens to list first is
// a coin flip that lands wrong half the time — and the patron has no way to know
// the better track was there, because the picker just shows two languages.
func TestAClosedCaptionTrackOutranksASubtitleTrack(t *testing.T) {
	m := mixedKindMachine(t, Options{})
	st := m.State()
	if got := st.CurrentLanguage; got != "en" {
		t.Fatalf("default language = %q, want %q — the subtitle track was listed first "+
			"in the RPL and won, so a deaf patron is served the track without the sound cues",
			got, "en")
	}
	if len(st.AvailableLanguages) != 2 {
		t.Fatalf("available languages = %v, want both offered", st.AvailableLanguages)
	}
}

// TestAVenuePreferenceStillOutranksTrackKind: accessibility ranking is step (3),
// AFTER the venue's configured order. A venue that has chosen French for its
// screens has made a decision about its audience that a track-kind heuristic
// must not quietly overturn.
func TestAVenuePreferenceStillOutranksTrackKind(t *testing.T) {
	m := mixedKindMachine(t, Options{PreferredLanguages: []string{"fr"}})
	if got := m.State().CurrentLanguage; got != "fr" {
		t.Fatalf("default language = %q, want %q — the venue's configured preference was overridden",
			got, "fr")
	}
}

// TestRankingNeverDropsALanguage: ordering is ordering. Whatever the RPL says
// about track kinds, every language the show carries must still be offered, or
// the ranking has silently removed a patron's option (guardrail #7).
func TestRankingNeverDropsALanguage(t *testing.T) {
	m := mixedKindMachine(t, Options{})
	got := map[string]bool{}
	for _, l := range m.State().AvailableLanguages {
		got[l] = true
	}
	for _, want := range []string{"en", "fr"} {
		if !got[want] {
			t.Fatalf("language %q disappeared from the offer: %v", want, m.State().AvailableLanguages)
		}
	}
}

// TestThePhoneOfAPatronWithNoPreferenceGetsTheCaptionTrack is the end of the
// chain, and the only step of it a patron ever experiences.
//
// The machine ranks the languages; internal/web's negotiation takes the head of
// that list when a patron has expressed no choice and their browser language
// matches nothing. Both halves have to agree or the ranking is decoration: the
// machine can prefer the closed-caption track all it likes, but if the offer
// list reaches the web layer in playlist order then the phone still shows the
// subtitle track. This test spans the seam, so reordering either side alone
// fails it.
func TestThePhoneOfAPatronWithNoPreferenceGetsTheCaptionTrack(t *testing.T) {
	m := mixedKindMachine(t, Options{})
	offered := m.State().AvailableLanguages
	// No venue default, and a browser asking for a language the show lacks.
	got, _, _ := web.NegotiateLanguageForTest("", []string{"pt-BR"}, "", offered)
	if got != "en" {
		t.Fatalf("the phone would be served %q, want %q — the closed-caption track "+
			"lost to the subtitle track somewhere between the RPL and the patron "+
			"(offer list was %v)", got, "en", offered)
	}
}
