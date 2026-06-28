package resolver

import (
	"context"
	"fmt"

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
// using the transient cache keyed by resource ID.
func (p *Pipeline) Document(ctx context.Context, res rpl.Resource) (*timedtext.Document, error) {
	if p.Cache != nil {
		if b, ok := p.Cache.Get(res.ID); ok {
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
		p.Cache.Put(res.ID, xml)
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

// LoadWindow fetches caption cues for the reels in the fetch-ahead window starting
// at currentReelIdx, for the requested languages (in priority order). Returns
// composition-relative cues per language plus a list of non-fatal warning classes
// (e.g. a language missing for a reel, or an unfetchable asset). Reels before
// currentReelIdx are skipped (never fetched — §5.3).
func (l *Loader) LoadWindow(ctx context.Context, r *rpl.RPL, currentReelIdx int, languages []string) (map[string][]model.Cue, []string, error) {
	if r == nil {
		return nil, nil, fmt.Errorf("nil rpl")
	}
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
		offset := r.ReelOffsetMS(reelIdx)
		reel := r.Reels[reelIdx]
		for _, lang := range languages {
			res, ok := reel.SelectResource(lang)
			if !ok {
				// No captions for this reel/language — legitimate (§5.2 item 5).
				continue
			}
			doc, err := l.Pipeline.Document(ctx, res)
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
