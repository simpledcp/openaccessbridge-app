package resolver

import (
	"context"
	"fmt"
	"strconv"

	"github.com/simpledcp/captionbridge/internal/caption/rpl"
	"github.com/simpledcp/captionbridge/internal/caption/timedtext"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
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

// Loader turns an RPL into composition-timeline-relative cues per language, with a
// bounded fetch-ahead window (§5.3): it fetches the current and next reel(s), not
// the whole composition, and skips reels already past.
type Loader struct {
	Pipeline        *Pipeline
	FetchAheadReels int // default 1 (current + next reel)
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
	return out, warnings, nil
}
