package resolver

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/caption/rpl"
	"github.com/simpledcp/openaccessbridge/internal/caption/timedtext"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Pipeline runs resolve → unwrap → (cache) → parse for one resource.
type Pipeline struct {
	Resolver Resolver
	Cache    *Cache
	Log      *logging.Logger
}

// NewPipeline builds a Pipeline.
func NewPipeline(r Resolver, cache *Cache, log *logging.Logger) *Pipeline {
	if log == nil {
		log = logging.Nop()
	}
	return &Pipeline{Resolver: r, Cache: cache, Log: log}
}

// Document fetches, unwraps, and parses a resource into a timed-text Document,
// using the transient cache. Equivalent to DocumentIn with an empty scope.
func (p *Pipeline) Document(ctx context.Context, res rpl.Resource) (*timedtext.Document, error) {
	return p.DocumentIn(ctx, "", res)
}

// DocumentIn is Document scoped to a content namespace — the playout / timeline
// segment the resource belongs to. Resource IDs are not dependably unique across
// shows (the same story as the Playout ID quirks in §5.2 item 4, one level down),
// so a cache keyed by resource ID alone can serve the previous show's timed text
// for the new show's resource. The key is (scope, resource ID, locator).
func (p *Pipeline) DocumentIn(ctx context.Context, scope string, res rpl.Resource) (*timedtext.Document, error) {
	key := scope + "|" + res.ID + "|" + res.Locator
	if p.Cache != nil {
		if b, ok := p.Cache.Get(key); ok {
			return timedtext.ParseBytes(b)
		}
	}
	raw, err := p.Resolver.Resolve(ctx, res.Locator)
	if err != nil {
		p.Log.Warn(logging.EvAssetFetchFailed,
			logging.Field("locator_form", locatorForm(res.Locator)),
			logging.Field("reason", "resolve_failed"))
		return nil, err
	}
	xml, err := Unwrap(raw)
	if err != nil {
		p.Log.Warn(logging.EvAssetFetchFailed,
			logging.Field("locator_form", locatorForm(res.Locator)),
			logging.Field("reason", "unwrap_failed"))
		return nil, err
	}
	if p.Cache != nil {
		p.Cache.Put(key, xml)
	}
	doc, err := timedtext.ParseBytes(xml)
	if err != nil {
		return nil, err
	}
	p.Log.Info(logging.EvAssetFetched,
		logging.Field("language", res.Language),
		logging.Field("kind", string(res.Kind)),
		logging.Field("cue_count", len(doc.Cues)))
	return doc, nil
}

// ReelScope namespaces a cached asset by (segment scope, reel index). Combined
// with the resource ID and locator in DocumentIn, a reused resource ID can never
// serve another show's — or another reel's — content, and eviction can be done a
// reel at a time (§5.3).
func ReelScope(scope string, reelIdx int) string {
	return scope + "#r" + strconv.Itoa(reelIdx)
}

// Purge empties the transient cache. Called at show boundaries (§5.3); nothing
// fetched outlives the show (guardrail #5).
func (p *Pipeline) Purge() {
	if p == nil || p.Cache == nil {
		return
	}
	p.Cache.Clear()
}

// EvAssetParseDegraded records that a timed-text asset parsed with non-fatal
// degradation — classes and counts only, never a cue. internal/logging's taxonomy
// is "the documented minimum set; callers may add events following the same dotted
// convention" (§9.4); its canonical home is internal/logging.
const EvAssetParseDegraded = logging.Event("caption.asset.parse_degraded")

// ParseWarning reports one asset's non-fatal timed-text parse degradation,
// attributed to the reel and language it came from (§5.4).
//
// The classes matter operationally: a defaulted edit rate, cues clamped to the
// <StartTime> origin, or a whole asset's cues falling before it all change what a
// patron sees. Dropping them on the floor is exactly the silent degradation
// guardrail #7 forbids. It carries CLASSES AND COUNTS ONLY — no cue text, no
// locator, no title-bearing identifier (§9.4).
type ParseWarning struct {
	ReelNumber int
	Language   string
	// Classes are the distinct warning classes, sorted so a dashboard/log line is
	// stable between loads.
	Classes []string
	// Count is the total number of warnings the document raised, which is how a
	// single stray cue is told apart from a whole asset going wrong.
	Count int
}

// LoadOutcome summarizes one fetch-ahead window load: what that load MEASURED
// about the content in the window, whether or not anything was wrong with it.
//
// It exists so a caller can CLEAR a content warning on evidence. "The playout ID
// changed" only says something changed; "the load that just ran found nothing
// degraded and nothing missing" is a measurement of the content now being served
// (§5.3, guardrail #7 — a condition must stop being reported because it passed,
// not because a moment went by).
type LoadOutcome struct {
	// Scope is the cache namespace the load ran under: unique to this RPL's
	// content, so two loads with the same scope describe the same show.
	Scope string
	// DegradedAssets is the number of assets that parsed with degradation, and
	// MissingAssets the number that would not serve at all. Counts only — the
	// classes travel on ParseWarning (§9.4).
	DegradedAssets int
	MissingAssets  int
}

// Loader turns an RPL into composition-timeline-relative cues per language, with a
// bounded fetch-ahead window (§5.3): it fetches the current and next reel(s), not
// the whole composition, and skips reels already past.
type Loader struct {
	Pipeline        *Pipeline
	FetchAheadReels int // default 1 (current + next reel)
	// OnParseWarning receives every asset's parse degradation as it is loaded, so
	// the condition can reach the dashboard instead of dying in the parser
	// (guardrail #7). It is deliberately NOT folded into Load's returned warning
	// list: that list means "this asset would not serve at all", and a degraded
	// asset that DID serve must not be reported as a missing one. nil discards.
	OnParseWarning func(ParseWarning)
	// OnLoadComplete, when set, is called once at the END of every window load —
	// after any OnParseWarning for that load — with what the load found. A caller
	// holding a live warning set uses it to drop a content warning the current
	// content no longer justifies. nil discards.
	OnLoadComplete func(LoadOutcome)
}

// NewLoader builds a Loader.
func NewLoader(p *Pipeline, fetchAhead int) *Loader {
	if fetchAhead < 0 {
		fetchAhead = 0
	}
	return &Loader{Pipeline: p, FetchAheadReels: fetchAhead}
}

// Purge empties the transient cache (show boundaries — §5.3).
func (l *Loader) Purge() {
	if l == nil {
		return
	}
	l.Pipeline.Purge()
}

// RetainWindow is the reel-boundary eviction (§5.3): it drops every cached asset
// outside the fetch-ahead window that starts at currentReelIdx for scope. Reels
// already played, and any other segment's or show's content, go; the reels still
// in the window stay, so crossing the boundary never re-fetches what was
// pre-fetched for it.
func (l *Loader) RetainWindow(scope string, currentReelIdx int) {
	if l == nil || l.Pipeline == nil || l.Pipeline.Cache == nil {
		return
	}
	if currentReelIdx < 0 {
		currentReelIdx = 0
	}
	prefixes := make([]string, 0, l.FetchAheadReels+1)
	for i := currentReelIdx; i <= currentReelIdx+l.FetchAheadReels; i++ {
		prefixes = append(prefixes, ReelScope(scope, i)+"|")
	}
	l.Pipeline.Cache.RetainOnly(prefixes)
}

// CachedBytes reports the transient cache size, so a caller (and the invariant
// tests) can assert that content really is gone at show end (guardrail #5).
func (l *Loader) CachedBytes() int64 {
	if l == nil || l.Pipeline == nil || l.Pipeline.Cache == nil {
		return 0
	}
	return l.Pipeline.Cache.Used()
}

// WindowRequest describes one fetch-ahead window load. Zero values reproduce the
// original LoadWindow behavior, so the request can be widened without disturbing
// callers.
type WindowRequest struct {
	RPL            *rpl.RPL
	CurrentReelIdx int
	Languages      []string // priority order (§5.3 language-prioritized fetch)
	// BaseOffsetMS is where this RPL starts on the composition timeline. An RPL is
	// one timeline SEGMENT, not necessarily the whole show (§5.3 "a map from
	// timeline segments → reel resources"), so a feature RPL that follows a trailer
	// RPL anchors its cues after the trailer rather than at zero.
	BaseOffsetMS int64
	// Scope namespaces the transient cache for this segment/playout so a resource
	// ID reused by another show cannot serve stale content.
	Scope string
}

// LoadWindow fetches caption cues for the reels in the fetch-ahead window starting
// at currentReelIdx, for the requested languages (in priority order).
func (l *Loader) LoadWindow(ctx context.Context, r *rpl.RPL, currentReelIdx int, languages []string) (map[string][]model.Cue, []string, error) {
	return l.Load(ctx, WindowRequest{RPL: r, CurrentReelIdx: currentReelIdx, Languages: languages})
}

// Load fetches the fetch-ahead window described by req. It returns
// composition-relative cues per language plus a list of non-fatal warning classes
// (e.g. a language missing for a reel, or an unfetchable asset). Reels before
// req.CurrentReelIdx are skipped — never fetched — so a mid-show reconnect does not
// pull content that will never play (§5.3).
func (l *Loader) Load(ctx context.Context, req WindowRequest) (map[string][]model.Cue, []string, error) {
	r := req.RPL
	if r == nil {
		return nil, nil, fmt.Errorf("nil rpl")
	}
	currentReelIdx := req.CurrentReelIdx
	if currentReelIdx < 0 {
		currentReelIdx = 0
	}
	last := currentReelIdx + l.FetchAheadReels
	if last >= len(r.Reels) {
		last = len(r.Reels) - 1
	}
	out := make(map[string][]model.Cue)
	var warnings []string
	degraded := 0

	for reelIdx := currentReelIdx; reelIdx <= last; reelIdx++ {
		reel := r.Reels[reelIdx]
		reelStart := req.BaseOffsetMS + r.ReelOffsetMS(reelIdx)
		for _, lang := range req.Languages {
			res, ok := reel.SelectResource(lang)
			if !ok {
				// No captions for this reel/language — legitimate (§5.2 item 5).
				continue
			}
			// Cue times are asset-relative; the entry point is where the reel
			// starts inside the asset, so a trimmed asset shifts back by it.
			offset := reelStart - reel.TrimMS(res)
			doc, err := l.Pipeline.DocumentIn(ctx, ReelScope(req.Scope, reelIdx), res)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("reel%d_%s_fetch_failed", reel.ReelNumber, lang))
				continue
			}
			if l.noteParseWarnings(reel.ReelNumber, lang, doc.Warnings) {
				degraded++
			}
			for _, c := range doc.Cues {
				c.StartMS += offset
				c.EndMS += offset
				c.Kind = res.Kind // authoritative ST 429-12 track type from the RPL
				c.Language = lang
				if c.ResourceID == "" {
					c.ResourceID = res.ID
				}
				out[lang] = append(out[lang], c)
			}
		}
	}
	if l.OnLoadComplete != nil {
		l.OnLoadComplete(LoadOutcome{
			Scope: req.Scope, DegradedAssets: degraded, MissingAssets: len(warnings),
		})
	}
	return out, warnings, nil
}

// noteParseWarnings reports one document's parse degradation to OnParseWarning
// and to the log, reporting whether the document was degraded at all. Repeats of
// a class are collapsed — a hundred cues clamped to the same origin is one
// condition with a count, not a hundred conditions.
func (l *Loader) noteParseWarnings(reelNumber int, lang string, classes []string) bool {
	if len(classes) == 0 {
		return false
	}
	seen := make(map[string]bool, len(classes))
	distinct := make([]string, 0, len(classes))
	for _, c := range classes {
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		distinct = append(distinct, c)
	}
	sort.Strings(distinct)
	if l.Pipeline != nil {
		l.Pipeline.Log.Warn(EvAssetParseDegraded,
			logging.Field("reel", reelNumber),
			logging.Field("language", lang),
			logging.Field("classes", strings.Join(distinct, ",")),
			logging.Field("count", len(classes)))
	}
	if l.OnParseWarning != nil {
		l.OnParseWarning(ParseWarning{
			ReelNumber: reelNumber, Language: lang,
			Classes: distinct, Count: len(classes),
		})
	}
	return true
}
