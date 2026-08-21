package resolver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/rpl"
	"github.com/simpledcp/openaccessbridge/internal/caption/timedtext"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

func ttDoc(lang, text string, inMS, outMS int) string {
	// Minimal SMPTE 428 doc with one cue at the given ms (rate 1000 → ms direct).
	// Using TimeCodeRate 1000 makes the editable unit a millisecond for easy math.
	tc := func(ms int) string {
		s := ms / 1000
		u := ms % 1000
		return formatTC(0, 0, s, u)
	}
	return `<?xml version="1.0"?><SubtitleReel xmlns="x"><Language>` + lang +
		`</Language><EditRate>1000 1</EditRate><TimeCodeRate>1000</TimeCodeRate><SubtitleList><Font>` +
		`<Subtitle TimeIn="` + tc(inMS) + `" TimeOut="` + tc(outMS) + `"><Text>` + text + `</Text></Subtitle>` +
		`</Font></SubtitleList></SubtitleReel>`
}

func formatTC(h, m, s, u int) string {
	return itoa2(h) + ":" + itoa2(m) + ":" + itoa2(s) + ":" + itoa3(u)
}
func itoa2(n int) string {
	if n < 10 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
func itoa3(n int) string {
	if n < 10 {
		return "00" + itoa(n)
	} else if n < 100 {
		return "0" + itoa(n)
	}
	return itoa(n)
}
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestLocatorForm(t *testing.T) {
	cases := map[string]string{
		"http://x/y":         "http",
		"https://x/y":        "https",
		"file:///srv/a.xml":  "file",
		"captions/reel1.xml": "server-relative",
		"dcp://vendor/asset": "scheme:dcp",
		"":                   "empty",
	}
	for in, want := range cases {
		if got := locatorForm(in); got != want {
			t.Errorf("locatorForm(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPluggableServerRelativeAndHTTP(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(ttDoc("en", "from-http", 1000, 2000)))
	}))
	defer ts.Close()

	mb := &MemoryFetcher{Files: map[string][]byte{
		"captions/reel1_en.xml": []byte(ttDoc("en", "from-mb", 1000, 2000)),
	}}
	p := NewPluggable(mb, ts.Client(), nil)

	b, err := p.Resolve(context.Background(), "captions/reel1_en.xml")
	if err != nil || len(b) == 0 {
		t.Fatalf("server-relative resolve: %v", err)
	}
	b2, err := p.Resolve(context.Background(), ts.URL+"/aux/x.xml")
	if err != nil || len(b2) == 0 {
		t.Fatalf("http resolve: %v", err)
	}
}

func TestPluggableUnresolvedFormClassified(t *testing.T) {
	p := NewPluggable(nil, nil, nil)
	_, err := p.Resolve(context.Background(), "dcp://vendor/secret_title_asset")
	if err == nil {
		t.Fatalf("expected error for vendor-specific locator")
	}
	got := p.UnresolvedLocators()
	if len(got) != 1 || got[0].Form != "scheme:dcp" || got[0].Count != 1 {
		t.Fatalf("expected classified unresolved form, got %+v", got)
	}
	if got[0].Family != "" {
		t.Fatalf("no server declared a family, so the observation must not claim one: %+v", got[0])
	}
}

// TestUnresolvedLocatorsAreAttributedToTheirServerFamily is the half §5.3 asks
// for and the resolver did not have: "log and classify every unresolved locator
// form BY SERVER FAMILY".
//
// The count alone cannot be acted on. Coverage is meant to grow from real
// observations (B1: the wire contract is unconfirmed), and an observation
// without its source does not compose — two venues reporting three unresolved
// `scheme:dcp` locators are either one vendor problem or two, and the form
// count cannot tell them apart. The family is the difference between "a form
// exists" and "this vendor emits it", which is a handler to write.
func TestUnresolvedLocatorsAreAttributedToTheirServerFamily(t *testing.T) {
	p := NewPluggable(nil, nil, nil)
	p.SetServerFamily("GDC")
	if _, err := p.Resolve(context.Background(), "dcp://vendor/a"); err == nil {
		t.Fatal("expected an error for a vendor-specific locator")
	}
	if _, err := p.Resolve(context.Background(), "dcp://vendor/b"); err == nil {
		t.Fatal("expected an error for a vendor-specific locator")
	}
	// A different server takes over the link mid-session.
	p.SetServerFamily("Dolby")
	if _, err := p.Resolve(context.Background(), "dcp://vendor/c"); err == nil {
		t.Fatal("expected an error for a vendor-specific locator")
	}
	got := p.UnresolvedLocators()
	if len(got) != 2 {
		t.Fatalf("two families emitted the form; got %d buckets: %+v", len(got), got)
	}
	// Stable order: family then form.
	if got[0].Family != "Dolby" || got[0].Count != 1 {
		t.Errorf("Dolby bucket = %+v, want 1 observation", got[0])
	}
	if got[1].Family != "GDC" || got[1].Count != 2 {
		t.Errorf("GDC bucket = %+v, want 2 observations", got[1])
	}
}

// The earlier observations keep the family they were recorded under. Attributing
// them to whoever announced last would invent evidence that a vendor emits a
// form nobody ever saw it emit — which is exactly the kind of claim a Phase 0A
// finding must not contain.
func TestChangingFamilyDoesNotReattributeEarlierObservations(t *testing.T) {
	p := NewPluggable(nil, nil, nil)
	p.SetServerFamily("GDC")
	if _, err := p.Resolve(context.Background(), "dcp://vendor/a"); err == nil {
		t.Fatal("expected an error")
	}
	p.SetServerFamily("Dolby")
	got := p.UnresolvedLocators()
	if len(got) != 1 || got[0].Family != "GDC" {
		t.Fatalf("the observation was re-attributed: %+v", got)
	}
}

// §9.4: the raw locator can embed a film title. The form is loggable; the path
// behind it never is — not in the log line, and not in the accumulation a
// dashboard renders.
func TestNeitherTheLogNorTheTallyCarriesTheLocator(t *testing.T) {
	var buf syncBuf
	p := NewPluggable(nil, nil, logging.New(logging.Options{Component: "test", JSON: true, Writer: &buf}))
	p.SetServerFamily("GDC")
	const secret = "THE_UNRELEASED_FILM_TITLE"
	if _, err := p.Resolve(context.Background(), "dcp://vendor/"+secret); err == nil {
		t.Fatal("expected an error")
	}
	if out := buf.String(); strings.Contains(out, secret) {
		t.Fatalf("the locator reached the log:\n%s", out)
	} else if !strings.Contains(out, "GDC") {
		t.Fatalf("the server family — operational configuration, not content — was not logged:\n%s", out)
	}
	for _, u := range p.UnresolvedLocators() {
		if strings.Contains(u.Form, secret) {
			t.Fatalf("the locator reached the tally: %+v", u)
		}
	}
}

// syncBuf is a concurrency-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestUnwrapPlainXML(t *testing.T) {
	in := []byte("  \xef\xbb\xbf<?xml version=\"1.0\"?><SubtitleReel/>")
	out, err := Unwrap(in)
	if err != nil {
		t.Fatalf("Unwrap plain: %v", err)
	}
	if out[0] != '<' {
		t.Fatalf("expected XML start, got %q", out[:1])
	}
}

func TestUnwrapMXFExtractsXML(t *testing.T) {
	xml := []byte(`<?xml version="1.0"?><SubtitleReel xmlns="x"><Language>en</Language></SubtitleReel>`)
	// Build a fake MXF: partition pack key prefix, filler, the XML essence, trailer.
	var mxf []byte
	mxf = append(mxf, 0x06, 0x0e, 0x2b, 0x34, 0x02, 0x05, 0x01, 0x01)
	mxf = append(mxf, make([]byte, 32)...) // filler/KLV length bytes
	mxf = append(mxf, xml...)
	mxf = append(mxf, 0x00, 0x01, 0x02, 0x03) // trailing KLV
	out, err := Unwrap(mxf)
	if err != nil {
		t.Fatalf("Unwrap MXF: %v", err)
	}
	if string(out) != string(xml) {
		t.Fatalf("MXF extract = %q", out)
	}
}

func TestUnwrapGarbageErrors(t *testing.T) {
	if _, err := Unwrap([]byte{0x00, 0x01, 0x02, 0x03, 0x04}); err == nil {
		t.Fatalf("expected error for non-XML non-MXF bytes")
	}
}

func TestCacheLRUEviction(t *testing.T) {
	c := NewCache(100)
	c.Put("a", make([]byte, 40))
	c.Put("b", make([]byte, 40))
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a should be present")
	}
	// Touch a so b is LRU, then add c (40): total 120 > 100 → evict LRU (b).
	c.Put("c", make([]byte, 40))
	if _, ok := c.Get("b"); ok {
		t.Fatalf("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatalf("a (recently used) should survive")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatalf("c (just added) should be present")
	}
	if c.Used() > 100 {
		t.Fatalf("cache over budget: %d", c.Used())
	}
	c.Clear()
	if c.Len() != 0 || c.Used() != 0 {
		t.Fatalf("clear failed: len=%d used=%d", c.Len(), c.Used())
	}
}

func TestCacheOversizeItemNotStored(t *testing.T) {
	c := NewCache(50)
	c.Put("big", make([]byte, 100))
	if _, ok := c.Get("big"); ok {
		t.Fatalf("oversize item should not be cached")
	}
}

func TestLoaderWindowOffsetsAndKind(t *testing.T) {
	r := &rpl.RPL{
		PlayoutID: "1",
		Reels: []rpl.Reel{
			{ReelNumber: 1, Resources: []rpl.Resource{{
				ID: "r1en", Kind: model.TrackClosedCaption, Language: "en-US",
				Locator:  "captions/reel1_en.xml",
				EditRate: rateUnits(24), IntrinsicDurationUnits: 1440, // 60000ms
			}}},
			{ReelNumber: 2, Resources: []rpl.Resource{{
				ID: "r2en", Kind: model.TrackClosedCaption, Language: "en-US",
				Locator:  "captions/reel2_en.xml",
				EditRate: rateUnits(24), IntrinsicDurationUnits: 1440,
			}}},
		},
	}
	mb := &MemoryFetcher{Files: map[string][]byte{
		"captions/reel1_en.xml": []byte(ttDoc("en-US", "R1", 1000, 2000)),
		"captions/reel2_en.xml": []byte(ttDoc("en-US", "R2", 500, 1500)),
	}}
	pipe := NewPipeline(NewPluggable(mb, nil, nil), NewCache(1<<20), nil)
	loader := NewLoader(pipe, 1)

	cues, warns, err := loader.LoadWindow(context.Background(), r, 0, []string{"en-US"})
	if err != nil {
		t.Fatalf("LoadWindow: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	got := cues["en-US"]
	if len(got) != 2 {
		t.Fatalf("expected 2 cues across 2 reels, got %d (%+v)", len(got), got)
	}
	// Reel 1 cue stays at 1000-2000; reel 2 cue offset by reel1 duration (60000).
	if got[0].StartMS != 1000 || got[0].Text != "R1" {
		t.Fatalf("reel1 cue = %+v", got[0])
	}
	if got[1].StartMS != 60500 || got[1].EndMS != 61500 || got[1].Text != "R2" {
		t.Fatalf("reel2 cue not offset correctly: %+v", got[1])
	}
	// Kind stamped from the RPL (authoritative ST 429-12 track type).
	if got[0].Kind != model.TrackClosedCaption {
		t.Fatalf("kind not stamped: %v", got[0].Kind)
	}
}

func TestLoaderSkipsPastReelsAndMissingLanguage(t *testing.T) {
	r := &rpl.RPL{
		Reels: []rpl.Reel{
			{ReelNumber: 1, Resources: []rpl.Resource{{ID: "r1", Kind: model.TrackClosedCaption, Language: "en", Locator: "r1.xml", EditRate: rateUnits(24), IntrinsicDurationUnits: 1440}}},
			{ReelNumber: 2, Resources: []rpl.Resource{{ID: "r2", Kind: model.TrackClosedCaption, Language: "en", Locator: "r2.xml", EditRate: rateUnits(24), IntrinsicDurationUnits: 1440}}},
		},
	}
	mb := &MemoryFetcher{Files: map[string][]byte{
		"r1.xml": []byte(ttDoc("en", "R1", 1000, 2000)),
		"r2.xml": []byte(ttDoc("en", "R2", 1000, 2000)),
	}}
	loader := NewLoader(NewPipeline(NewPluggable(mb, nil, nil), nil, nil), 0)

	// Start at reel index 1 with fetch-ahead 0 → only reel 2 fetched, reel 1 skipped.
	cues, _, _ := loader.LoadWindow(context.Background(), r, 1, []string{"en"})
	if len(cues["en"]) != 1 || cues["en"][0].Text != "R2" {
		t.Fatalf("expected only reel2, got %+v", cues["en"])
	}

	// Missing language → warning, not error.
	_, warns, err := loader.LoadWindow(context.Background(), r, 0, []string{"de"})
	if err != nil {
		t.Fatalf("missing language should not error: %v", err)
	}
	_ = warns // SelectResource simply returns no resource for "de"; no fetch attempted
}

func rateUnits(fps int) timedtext.EditRate { return timedtext.EditRate{Num: fps, Den: 1} }
