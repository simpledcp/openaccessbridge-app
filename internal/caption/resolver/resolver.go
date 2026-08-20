// Package resolver fetches the timed-text resources an RPL references and
// normalizes them into cues (buildplan.md §5.3). Resource locators are a
// compatibility surface, not a solved abstraction, so the fetcher is a PLUGGABLE
// resolver with handlers per locator form, plus a mandatory track-file/MXF
// (ST 429-5) unwrap stage — built even though the first target server may return
// plain XML, so a track-file-returning server is a configuration case, not a
// rewrite.
//
// The full pipeline is:
//
//	resolver (per-locator handler) → optional MXF/track-file unwrap
//	  → timed-text XML → cue normalization (timedtext) → reel/timeline anchoring
//
// Nothing fetched is persisted: the cache is in-memory only and purged at reel and
// show boundaries (guardrail #5).
package resolver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/logging"
)

// Resolver fetches the raw bytes for a locator. The bytes may be plain timed-text
// XML, a track-file reference, or an MXF-wrapped track (handled by Unwrap).
type Resolver interface {
	Resolve(ctx context.Context, locator string) ([]byte, error)
}

// MediaBlockFetcher fetches server-relative / file-like locators from the media
// block over the ACSP-compatible mechanism. In production this is the real media
// block; in tests it is a MemoryFetcher.
type MediaBlockFetcher interface {
	Fetch(ctx context.Context, ref string) ([]byte, error)
}

// MemoryFetcher serves locators from an in-memory map (tests + the simulator).
type MemoryFetcher struct {
	Files map[string][]byte
}

// Fetch returns the bytes for ref or an error if absent (simulating an
// unfetchable asset).
func (m *MemoryFetcher) Fetch(_ context.Context, ref string) ([]byte, error) {
	if b, ok := m.Files[ref]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("asset not found: %s", classify(ref))
}

// maxAssetBytes bounds a single fetched asset to protect appliance memory.
const maxAssetBytes = 8 << 20 // 8 MiB

// DefaultHTTPTimeout bounds a single HTTP asset fetch. An unresponsive media block
// must never hang a fetch forever: asset fetches run on the ACSP request path, and
// a stalled fetch there starves the lease/comms checks the caption link depends on
// (§5.2 items 1-2, §5.3 "on demand"). http.DefaultClient has NO timeout, so it is
// never used here.
const DefaultHTTPTimeout = 15 * time.Second

// Pluggable dispatches a locator to the right handler by form, and classifies +
// logs any unresolved (vendor-specific) form by class — never the full locator,
// which can embed a title (§9.4).
type Pluggable struct {
	HTTP       *http.Client
	MediaBlock MediaBlockFetcher
	Log        *logging.Logger

	mu         sync.Mutex
	unresolved map[string]int
}

// NewPluggable builds a Pluggable resolver.
func NewPluggable(mb MediaBlockFetcher, httpc *http.Client, log *logging.Logger) *Pluggable {
	if httpc == nil {
		httpc = &http.Client{Timeout: DefaultHTTPTimeout}
	}
	if log == nil {
		log = logging.Nop()
	}
	return &Pluggable{HTTP: httpc, MediaBlock: mb, Log: log, unresolved: map[string]int{}}
}

// Resolve fetches a locator using the appropriate handler.
func (p *Pluggable) Resolve(ctx context.Context, locator string) ([]byte, error) {
	form := locatorForm(locator)
	switch form {
	case "http", "https":
		return p.resolveHTTP(ctx, locator)
	case "server-relative", "file":
		if p.MediaBlock == nil {
			return nil, fmt.Errorf("no media-block fetcher configured for %s locator", form)
		}
		return p.MediaBlock.Fetch(ctx, locator)
	default:
		// Vendor-specific / unknown: log + classify by FORM, never drop silently
		// and never log the raw locator (it can embed a title).
		p.mu.Lock()
		p.unresolved[form]++
		p.mu.Unlock()
		p.Log.Warn(logging.EvAssetFetchFailed,
			logging.Field("locator_form", form),
			logging.Field("reason", "unresolved_locator_form"))
		return nil, fmt.Errorf("unresolved locator form %q", form)
	}
}

func (p *Pluggable) resolveHTTP(ctx context.Context, locator string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, locator, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: status %d", classify(locator), resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes))
}

// UnresolvedForms returns a snapshot of unresolved locator-form counts, seeded by
// real observations so resolver coverage grows from the field (§5.3 / Phase 0A).
func (p *Pluggable) UnresolvedForms() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.unresolved))
	for k, v := range p.unresolved {
		out[k] = v
	}
	return out
}

// locatorForm classifies a locator into a coarse form for dispatch + telemetry.
func locatorForm(locator string) string {
	l := strings.TrimSpace(locator)
	if l == "" {
		return "empty"
	}
	if u, err := url.Parse(l); err == nil && u.Scheme != "" {
		switch strings.ToLower(u.Scheme) {
		case "http":
			return "http"
		case "https":
			return "https"
		case "file":
			return "file"
		default:
			return "scheme:" + strings.ToLower(u.Scheme)
		}
	}
	// No scheme: a server-relative reference or file-like path served by the
	// media block.
	return "server-relative"
}

// classify returns a loggable, non-content description of a locator: its form and
// length only, never the raw path (which can embed a title, §9.4).
func classify(locator string) string {
	return fmt.Sprintf("[%s len=%d]", locatorForm(locator), len(locator))
}
