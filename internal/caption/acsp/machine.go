package acsp

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/resolver"
	"github.com/simpledcp/captionbridge/internal/caption/rpl"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// WarnLeaseNearExpiry is raised while the lease is close to lapsing — §5.2 item 1
// ("Treat anything approaching the lease as a warning"). It is emitted through
// OnWarning so the condition reaches the dashboard instead of living only in a log
// line. It is declared next to the state machine that raises it; it belongs beside
// the other Warn* codes in internal/model once the readiness assembler grows an
// input for caption-link warnings.
const WarnLeaseNearExpiry = "caption_lease_near_expiry"

// WarnCaptionAssetUnavailable is raised when the media block will not serve a
// timed-text resource the RPL names. The patron sees that language go blank, so
// the condition has to be loud (guardrail #7 — no silent degradation — and §8.3
// "the dashboard must shout"); a log line alone is exactly the failure mode
// WarnLeaseNearExpiry above was raised for. Same canonical home in internal/model.
const WarnCaptionAssetUnavailable = "caption_asset_unavailable"

// defaultFetchTimeout bounds one RPL / timed-text fetch made from the request path.
const defaultFetchTimeout = 5 * time.Second

// Options configures a Machine.
type Options struct {
	Controller          *caption.Controller
	Resolver            resolver.Resolver // fetches the RPL document
	Loader              *resolver.Loader  // fetches timed-text per reel
	Clock               func() time.Time
	Log                 *logging.Logger
	ExpectedFingerprint string // wrong-room guard (§8.5); empty disables it
	// ExpectedScreenName is the OTHER half of §8.5's pinned identity: the screen
	// name this auditorium profile expects the server to announce. Empty disables
	// it. It is the weakest of the three pins and the one most likely to be
	// available, since §10.5's composite identity is "configured IP + server
	// family + screen name" — so it is often the only wrong-room signal there is.
	ExpectedScreenName string
	PreferredLanguages []string // default-language priority (§5.4 / D8)
	// WarnFraction: emit a lease-expiring warning when remaining < WarnFraction of
	// the lease. Default 0.25.
	WarnFraction float64
	OnState      func(model.CaptionLinkState)
	OnWarning    func(model.Warning)
	// FetchAheadReels overrides the loader's window when (re)loading reels.
	FetchAheadReels int
	// EagerLanguages caps how many languages are fetched up front, in D8 priority
	// order — the "language-prioritized fetch" of §5.3. 0 (the default) fetches
	// every available language, which is what the patron language picker relies on
	// until the fan-out drives RequestLanguage on a language switch; setting it
	// bounds the transient cache but REQUIRES that wiring, or a patron who selects
	// a language outside the cap sees nothing.
	EagerLanguages int
	// FetchTimeout bounds one RPL / asset fetch. It is further clamped to the
	// remaining lease, so a hung media block can never push a fetch past the lease
	// it is fetching for (§5.2 items 1-2). Default 5s.
	FetchTimeout time.Duration
	// OnSelfHeal is called when the lease genuinely expires, so a connection
	// supervisor can re-run the full connection sequence rather than leaving the
	// ACS in a dead state (§5.2 item 6). See Supervisor.
	OnSelfHeal func()
}

// Machine is the ST 430-10 ACS state machine. The Handle/Tick API is synchronous
// and clock-injectable so the scenario suite is deterministic and CI-able; Run
// wraps them in a production loop over a Transport.
//
// All exported methods are safe for concurrent use: the dashboard reads State
// while the link goroutine drives Handle/Tick. Callbacks (OnState, OnWarning,
// OnSelfHeal) are invoked with the lock held and must not call back into the
// Machine.
type Machine struct {
	mu        sync.Mutex
	ctrl      *caption.Controller
	res       resolver.Resolver
	loader    *resolver.Loader
	clock     func() time.Time
	log       *logging.Logger
	expFP     string
	expScreen string
	prefLng   []string
	warnFr    float64
	onState   func(model.CaptionLinkState)
	onWarn    func(model.Warning)
	onHeal    func()
	eagerN    int
	fetchTO   time.Duration

	state         model.ACSPState
	server        string
	screenName    string
	leaseActive   bool
	leaseMS       int64
	leaseDeadline time.Time
	lastRequest   time.Time
	outputMode    model.OutputMode
	playoutID     string
	timelineMS    int64
	playState     PlayState
	languages     []string // full D8 priority order over the available languages
	loadedLangs   []string // the subset actually fetched (eager set + on demand)
	reqLangs      []string // languages patrons asked for beyond the eager set
	availLangs    []string
	segments      []rplSegment // RPLs keyed by timeline segment (§5.3)
	activeSeg     int          // index into segments, -1 = none
	currentReel   int          // reel index within the active segment
	currentRPL    *rpl.RPL
	commsLost     bool
	leaseWarned   bool
	identity      IdentityState
	runCtx        context.Context
}

// New builds a Machine.
func New(opts Options) *Machine {
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	log := opts.Log
	if log == nil {
		log = logging.Nop()
	}
	wf := opts.WarnFraction
	if wf <= 0 {
		wf = 0.25
	}
	to := opts.FetchTimeout
	if to <= 0 {
		to = defaultFetchTimeout
	}
	if opts.FetchAheadReels > 0 && opts.Loader != nil {
		opts.Loader.FetchAheadReels = opts.FetchAheadReels
	}
	return &Machine{
		ctrl:       opts.Controller,
		res:        opts.Resolver,
		loader:     opts.Loader,
		clock:      clk,
		log:        log,
		expFP:      opts.ExpectedFingerprint,
		expScreen:  strings.TrimSpace(opts.ExpectedScreenName),
		prefLng:    opts.PreferredLanguages,
		warnFr:     wf,
		onState:    opts.OnState,
		onWarn:     opts.OnWarning,
		onHeal:     opts.OnSelfHeal,
		eagerN:     opts.EagerLanguages,
		fetchTO:    to,
		state:      model.StateDisconnected,
		outputMode: model.OutputUnknown,
		activeSeg:  -1,
		identity:   IdentityUnknown,
	}
}

// Connected marks the TCP connection established (Variant A: the ACS dialed the
// server). The server then drives the exchange.
func (m *Machine) Connected() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = model.StateConnected
	m.lastRequest = m.clock()
	m.log.Info(logging.EvACSPConnected)
	m.emitState()
}

// Handle processes one server→ACS request and returns the ACS response. It is the
// single place protocol requests mutate state and drive the caption controller.
func (m *Machine) Handle(req Request) Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	m.lastRequest = now
	if m.commsLost {
		// A request arrived: the momentary interruption is over (§5.2 item 2).
		m.commsLost = false
		m.ctrl.CommsRestored()
		m.log.Info(logging.EvACSPConnected, logging.Field("reason", "comms_restored"))
	}

	switch req.Type {
	case ReqAnnounce:
		return m.handleAnnounce(req)
	case ReqGetNewLease:
		return m.handleGetNewLease(req, now)
	case ReqStatus:
		return m.handleStatus(req, now)
	case ReqUpdateTimeline:
		return m.handleUpdateTimeline(req)
	case ReqSetRPLLocation:
		return m.handleSetRPL(req)
	case ReqSetOutputMode:
		return m.handleSetOutputMode(req)
	case ReqTerminateLease:
		return m.handleTerminate(req)
	default:
		return Response{Type: req.Type, OK: false, Reason: "unknown_request"}
	}
}

func (m *Machine) handleAnnounce(req Request) Response {
	m.server = req.ServerID
	m.screenName = req.ScreenName
	if m.state == model.StateDisconnected {
		m.state = model.StateConnected
	}
	m.state = model.StateAnnounced
	m.checkIdentity(req)
	m.emitState()
	return Response{Type: ReqAnnounce, OK: true, State: string(m.state)}
}

// checkIdentity applies the §8.5 wrong-room guard: "pin the expected server
// identity per auditorium profile (expected IP, ACSP server fingerprint, and
// screen name); warn loudly if the connected server does not match."
//
// It reports THREE outcomes, not two. "Nothing was pinned" and "something was
// pinned but the server announced nothing to check it against" are both
// UNVERIFIED, and neither may render as a confirmed match — the appliance would
// otherwise show a green wrong-room guard it never actually performed, which is
// the same class of defect as reporting a firewall active without reading the
// ruleset (§9.3). A mismatch is loud but is NOT cryptographic authentication
// (§10.5): the pin may be a weak composite identity, so the message says what
// failed rather than asserting the server is an impostor.
func (m *Machine) checkIdentity(req Request) {
	fpPinned, screenPinned := m.expFP != "", m.expScreen != ""
	if !fpPinned && !screenPinned {
		m.identity = IdentityUnpinned
		return
	}

	var mismatched []string
	checked := 0
	if fpPinned {
		if req.Fingerprint == "" {
			// Pinned but unanswerable. Phase 0A determines what each server
			// family actually exposes (B1); until then this is a real state.
			m.log.Warn(logging.EvServerIdentityMismatch, logging.Field("reason", "fingerprint_not_announced"))
		} else {
			checked++
			if req.Fingerprint != m.expFP {
				mismatched = append(mismatched, "fingerprint")
			}
		}
	}
	if screenPinned {
		if strings.TrimSpace(req.ScreenName) == "" {
			m.log.Warn(logging.EvServerIdentityMismatch, logging.Field("reason", "screen_name_not_announced"))
		} else {
			checked++
			// Operator-typed on one side, server-reported on the other, so
			// compare case- and whitespace-insensitively. Anything cleverer
			// (treating "Screen 3" as "Auditorium 3") would silence exactly the
			// re-cabling this guard exists to catch.
			if !strings.EqualFold(strings.TrimSpace(req.ScreenName), m.expScreen) {
				mismatched = append(mismatched, "screen name")
			}
		}
	}

	switch {
	case len(mismatched) > 0:
		m.identity = IdentityMismatch
		m.log.Warn(logging.EvServerIdentityMismatch,
			logging.Field("reason", "identity_mismatch"),
			logging.Field("mismatched", strings.Join(mismatched, ",")))
		m.warn(model.Warning{Code: model.WarnServerIdentityMismatch,
			Message: "Connected cinema server does not match this auditorium's pinned " +
				strings.Join(mismatched, " or ") + " — check the appliance is cabled to the right room.",
			Severity: model.SeverityCritical})
	case checked == 0:
		// Everything pinned went unannounced: the guard did not run.
		m.identity = IdentityUnverified
		m.warn(model.Warning{Code: model.WarnServerIdentityMismatch,
			Message: "This auditorium pins a server identity, but the connected server announced none of it — " +
				"the wrong-room check could not be performed.",
			Severity: model.SeverityWarning})
	default:
		m.identity = IdentityMatch
	}
}

func (m *Machine) handleGetNewLease(req Request, now time.Time) Response {
	m.leaseMS = req.LeaseMS
	if m.leaseMS <= 0 {
		m.leaseMS = 30000
	}
	m.leaseActive = true
	m.leaseDeadline = now.Add(time.Duration(m.leaseMS) * time.Millisecond)
	m.leaseWarned = false
	m.state = model.StateLeased
	m.log.Info(logging.EvLeaseGranted, logging.Field("lease_ms", m.leaseMS))
	m.emitState()
	return Response{Type: ReqGetNewLease, OK: true, State: string(m.state)}
}

func (m *Machine) handleStatus(req Request, now time.Time) Response {
	// A periodic status request renews the lease (half-lease cadence keepalive).
	switch {
	case m.leaseActive:
		ms := req.LeaseMS
		if ms <= 0 {
			ms = m.leaseMS
		}
		m.leaseDeadline = now.Add(time.Duration(ms) * time.Millisecond)
		m.leaseWarned = false
	case m.state == model.StateLeaseExpired && req.LeaseMS > 0:
		// Self-heal (§5.2 item 6): the server is still talking to us and is still
		// telling us the lease interval, so re-arm rather than sitting in a dead
		// state. Captions stay blank until the server re-anchors the timeline and
		// re-sends the RPL — the rest of the connection sequence.
		m.leaseMS = req.LeaseMS
		m.leaseActive = true
		m.leaseDeadline = now.Add(time.Duration(m.leaseMS) * time.Millisecond)
		m.leaseWarned = false
		m.state = model.StateLeased
		m.log.Info(logging.EvLeaseGranted,
			logging.Field("lease_ms", m.leaseMS),
			logging.Field("reason", "self_heal_after_expiry"))
		m.emitState()
	}
	// The periodic status carries lease AND timeline state (§5.2 request types); a
	// status with no timeline field stays a pure keepalive.
	if req.TimelineMS > 0 || req.PlayState != "" {
		if req.TimelineMS <= 0 {
			// A status that reports only the play state is NOT a seek to zero.
			// Taking it as one rewinds the show, re-anchors every engine at the
			// head and drags the fetch-ahead window back to reel 1 — a silent
			// wrong-content failure on any server that pushes play state more often
			// than position. Update Timeline stays the only request that moves the
			// anchor (§5.2 request types).
			req.TimelineMS = m.timelineMS
		}
		m.applyTimeline(req)
	}
	return Response{Type: ReqStatus, OK: true, State: string(m.state)}
}

func (m *Machine) handleUpdateTimeline(req Request) Response {
	m.applyTimeline(req)
	return Response{Type: ReqUpdateTimeline, OK: true, State: string(m.state)}
}

// applyTimeline re-anchors the controller on a server timeline value and slides the
// content window onto the reel now playing. It is shared by Update Timeline and by
// the periodic status, which carries the same timeline state (§5.2).
func (m *Machine) applyTimeline(req Request) {
	m.timelineMS = req.TimelineMS
	m.playState = req.PlayState
	if req.PlayoutID != "" {
		m.playoutID = req.PlayoutID
	}

	// Playout-ID quirk (§5.2 item 4): a repeated or zero Playout ID with an
	// advancing timeline is NOT a loop-back / seek-to-start. Disambiguate by
	// timeline position, never by Playout ID alone. We therefore drive the
	// controller purely from the timeline value and play state.
	switch req.PlayState {
	case PlayPlaying:
		if m.state == model.StatePaused {
			m.ctrl.Resume()
		}
		if m.state != model.StatePlaying {
			m.ctrl.Play(req.TimelineMS)
		} else {
			m.ctrl.UpdateTimeline(req.TimelineMS)
		}
		m.advanceToPlaying()
	case PlayPaused:
		m.ctrl.Pause()
		m.state = model.StatePaused
	case PlayStopped:
		m.ctrl.Stop()
		m.state = model.StateStopped
	default:
		// No play-state given: treat as a timeline re-anchor while playing.
		if m.state == model.StatePlaying {
			m.ctrl.UpdateTimeline(req.TimelineMS)
		}
	}
	if m.state == model.StateLeased {
		m.state = model.StateTimelineActive
	}
	// Honor the timeline for content, not just for cue resolution: pick the RPL
	// segment covering this position and move the fetch-ahead window onto the reel
	// now playing (§5.3).
	m.syncContent(false)
	m.emitState()
}

// advanceToPlaying moves to PLAYING once RPL+output allow it.
func (m *Machine) advanceToPlaying() {
	if m.outputMode == model.OutputDisabled {
		m.state = model.StatePlaying // playing transport, but captions suppressed
		m.blankOutput()
		m.ctrl.SetPatronStatus(model.PatronCaptionNoCaptions)
		return
	}
	m.state = model.StatePlaying
}

func (m *Machine) handleSetRPL(req Request) Response {
	var data []byte
	var err error
	if len(req.RPLInline) > 0 {
		data = req.RPLInline
	} else if req.RPLLocation != "" && m.res != nil {
		ctx, cancel := m.fetchCtx()
		data, err = m.res.Resolve(ctx, req.RPLLocation)
		cancel()
	}
	// A playlist the appliance cannot load is a caption-plane fault, and it used
	// to produce ONE LOG LINE: no state change, no warning, nothing on the
	// dashboard. The link went on reporting whatever it had been, and the next
	// PlayPlaying advanced it to PLAYING with nothing loaded — a patron shown
	// "active" over a blank area. Guardrail #7 is not audio-only.
	if err != nil || len(data) == 0 {
		m.log.Warn(logging.EvAssetFetchFailed, logging.Field("reason", "rpl_fetch_failed"))
		m.warn(model.Warning{Code: model.WarnCaptionRPLUnavailable,
			Message: "The cinema server named a caption playlist this appliance could not fetch. " +
				"Captions cannot start until it loads.",
			Severity: model.SeverityCritical})
		return Response{Type: ReqSetRPLLocation, OK: false, Reason: "rpl_fetch_failed"}
	}
	parsed, err := rpl.ParseBytes(data)
	if err != nil {
		m.log.Warn(logging.EvAssetFetchFailed, logging.Field("reason", "rpl_parse_failed"))
		m.warn(model.Warning{Code: model.WarnCaptionRPLUnavailable,
			Message: "The cinema server's caption playlist could not be read. This is a fault in " +
				"the playlist itself, not in the connection.",
			Severity: model.SeverityCritical})
		return Response{Type: ReqSetRPLLocation, OK: false, Reason: "rpl_parse_failed"}
	}
	// Store as a timeline segment. The Playout ID is a hint, never the key: it can
	// be zero or repeated (§5.2 item 4), and one connection can carry several RPLs
	// covering consecutive segments — see segments.go.
	idx := m.putSegment(parsed, fingerprint(data))
	m.log.Info(logging.EvRPLReceived,
		logging.Field("playout_id_present", parsed.PlayoutID != ""),
		logging.Field("reel_count", len(parsed.Reels)),
		logging.Field("segment_start_ms", m.segments[idx].startMS),
		logging.Field("segment_count", len(m.segments)),
		logging.Field("language_count", len(parsed.Languages())))

	m.syncContent(true)
	if m.state == model.StateTimelineActive || m.state == model.StateLeased {
		m.state = model.StateRPLReceived
	}
	if m.state == model.StateRPLReceived {
		m.state = model.StateReady
	}
	m.emitState()
	return Response{Type: ReqSetRPLLocation, OK: true, State: string(m.state)}
}

// syncContent selects the RPL segment covering the current timeline position and
// slides the fetch-ahead window onto the reel now playing. Crossing a segment or
// reel boundary evicts the assets outside the new window first (§5.3 "eviction at
// reel and show boundaries"), so a reel already played is not held. force reloads
// even when neither changed (a fresh RPL, or output being re-enabled).
func (m *Machine) syncContent(force bool) {
	idx := m.selectSegment(m.timelineMS, m.playoutID)
	if idx < 0 {
		return
	}
	seg := m.segments[idx]
	reel := seg.doc.ReelIndexAt(m.timelineMS - seg.startMS)
	boundary := idx != m.activeSeg || reel != m.currentReel
	if !boundary && !force {
		return
	}
	m.activeSeg = idx
	m.currentRPL = seg.doc
	m.currentReel = reel
	if boundary {
		m.loader.RetainWindow(seg.scope(), reel)
		m.log.Info(logging.EvStateTransition,
			logging.Field("phase", "content_window_advanced"),
			logging.Field("reel_index", reel),
			logging.Field("segment_start_ms", seg.startMS))
	}
	m.refreshLanguages()
	m.loadReels()
}

// refreshLanguages recomputes the available/priority/eager language sets from the
// active segment's RPL (§5.4 / D8).
func (m *Machine) refreshLanguages() {
	if m.activeSeg < 0 {
		m.availLangs, m.languages, m.loadedLangs = nil, nil, nil
		return
	}
	m.availLangs = m.segments[m.activeSeg].doc.Languages()
	m.languages = m.selectLanguages(m.availLangs)
	m.loadedLangs = m.eagerLanguages(m.languages)
	// A language a patron asked for stays fetched across reel and segment
	// boundaries — dropping it back to the eager set at the next reel would blank
	// that patron's phone with no visible cause (guardrail #7: no silent
	// degradation). Requests for a language the new RPL does not carry are simply
	// not loaded; the request itself is kept, not content, so it survives a purge.
	for _, l := range m.reqLangs {
		if !contains(m.availLangs, l) || contains(m.loadedLangs, l) {
			continue
		}
		m.loadedLangs = append(m.loadedLangs, l)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// loadReels (re)loads the fetch-ahead window into the controller for the loaded
// languages. An empty RPL (no caption resources) yields no cues — "no captions for
// this segment," not an error (§5.2 item 5).
func (m *Machine) loadReels() {
	if m.activeSeg < 0 || m.loader == nil {
		return
	}
	if m.outputMode == model.OutputDisabled {
		// The server says this content is not captioned: nothing is published,
		// and nothing is fetched for it either (§5.2 item 3).
		m.blankOutput()
		return
	}
	seg := m.segments[m.activeSeg]
	ctx, cancel := m.fetchCtx()
	cues, warns, err := m.loader.Load(ctx, resolver.WindowRequest{
		RPL:            seg.doc,
		CurrentReelIdx: m.currentReel,
		Languages:      m.loadedLangs,
		BaseOffsetMS:   seg.startMS,
		Scope:          seg.scope(),
	})
	cancel()
	if err != nil {
		return
	}
	for range warns {
		m.log.Warn(logging.EvAssetFetchFailed, logging.Field("reason", "reel_asset_unavailable"))
	}
	if len(warns) > 0 {
		// One warning per load, not one per resource: the condition is "this show's
		// captions are incomplete", and the count is the detail. The message carries
		// no locator and no caption text (§9.4).
		m.warn(model.Warning{Code: WarnCaptionAssetUnavailable,
			Message:  "The cinema server did not serve every caption asset for this content; some languages may be blank.",
			Severity: model.SeverityWarning})
	}
	m.ctrl.LoadReel(cues)
}

// blankOutput suppresses caption output without disturbing the timeline: every
// loaded language keeps its engine — and therefore its play state and anchor — but
// is given an empty cue set, so nothing is published to patrons while the server
// says this content is not captioned (§5.2 item 3). Restored by loadReels.
func (m *Machine) blankOutput() {
	if m.ctrl == nil {
		return
	}
	blank := make(map[string][]model.Cue, len(m.loadedLangs))
	for _, l := range m.loadedLangs {
		blank[l] = nil
	}
	m.ctrl.LoadReel(blank)
}

// fetchCtx bounds one fetch made from the request path. Asset fetches run in the
// same goroutine as the lease/comms checks, so an unresponsive media block must not
// be able to stall them: the deadline is the configured fetch timeout, clamped to
// the remaining lease — a fetch may never outlive the lease it is fetching for
// (§5.2 items 1-2, §5.3 "on demand").
func (m *Machine) fetchCtx() (context.Context, context.CancelFunc) {
	parent := m.runCtx
	if parent == nil {
		parent = context.Background()
	}
	d := m.fetchTO
	if m.leaseActive {
		if rem := m.leaseDeadline.Sub(m.clock()); rem > 0 && rem < d {
			d = rem
		}
	}
	if d <= 0 {
		d = defaultFetchTimeout
	}
	return context.WithTimeout(parent, d)
}

func (m *Machine) handleSetOutputMode(req Request) Response {
	prev := m.outputMode
	if req.OutputEnabled == nil {
		m.outputMode = model.OutputUnknown
	} else if *req.OutputEnabled {
		m.outputMode = model.OutputEnabled
	} else {
		m.outputMode = model.OutputDisabled
	}
	m.log.Info(logging.EvOutputModeChange, logging.Field("output_mode", string(m.outputMode)))
	switch m.outputMode {
	case model.OutputDisabled:
		// Not just a relabeled status: the caption text itself stops going out.
		m.blankOutput()
		m.ctrl.SetPatronStatus(model.PatronCaptionNoCaptions)
	case model.OutputEnabled:
		if prev == model.OutputDisabled {
			m.loadReels() // restore the window's cues; a repeat of "enabled" refetches nothing
		}
		if m.state == model.StatePlaying {
			m.ctrl.SetPatronStatus(model.PatronCaptionActive)
		}
	}
	m.emitState()
	return Response{Type: ReqSetOutputMode, OK: true, State: string(m.state)}
}

// handleTerminate is the new-show boundary: drop the abandoned previous show's
// content so the ACS does not keep holding it (§5.2 new-show-load). The server is
// expected to follow with Get New Lease → Update Timeline → Set RPL Location.
func (m *Machine) handleTerminate(req Request) Response {
	m.leaseActive = false
	m.purgeContent()
	m.ctrl.Stop()
	m.ctrl.LoadReel(map[string][]model.Cue{}) // retire all language engines
	m.state = model.StateShowChange
	m.log.Info(logging.EvStateTransition, logging.Field("phase", "show_change"))
	m.emitState()
	return Response{Type: ReqTerminateLease, OK: true, State: string(m.state)}
}

// purgeContent drops every trace of the current show from memory: the RPL segments
// and the transient timed-text cache behind them. Guardrail #5 — caption content
// lives in transient memory and is purged at show end — is only true if something
// actually calls this (§5.1, §5.3, §12.3).
func (m *Machine) purgeContent() {
	m.segments = nil
	m.activeSeg = -1
	m.currentReel = 0
	m.currentRPL = nil
	m.availLangs = nil
	m.languages = nil
	m.loadedLangs = nil
	m.playoutID = ""
	// The timeline anchor belonged to the show that just ended. Keeping it would
	// let a stale position place the NEXT show's first RPL (nextSegmentStart reads
	// it to spot a mid-show reconnect), and would report the old position on the
	// dashboard until the server re-anchors.
	m.timelineMS = 0
	m.playState = ""
	m.loader.Purge()
}

// Tick performs time-based checks: lease expiry, lease-expiring warning, and
// comms-loss detection. Production calls it on a ticker; tests call it after
// advancing the injected clock.
func (m *Machine) Tick() {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.clock()
	if !m.leaseActive {
		return
	}
	remaining := m.leaseDeadline.Sub(now)
	if remaining <= 0 {
		// Lease genuinely expired → blank captions, drop the show's content, and
		// ask for a fresh connection sequence rather than sitting dead (§5.2 #6).
		m.leaseActive = false
		m.state = model.StateLeaseExpired
		m.ctrl.LeaseExpired()
		m.purgeContent()
		m.log.Warn(logging.EvLeaseExpired)
		m.emitState()
		if m.onHeal != nil {
			m.onHeal()
		}
		return
	}
	if !m.leaseWarned && remaining < time.Duration(float64(m.leaseMS)*m.warnFr)*time.Millisecond {
		m.leaseWarned = true
		m.log.Warn(logging.EvLeaseExpiring, logging.Field("remaining_ms", remaining.Milliseconds()))
		m.warn(model.Warning{Code: WarnLeaseNearExpiry,
			Message:  "Cinema server status requests are arriving close to the caption lease expiry.",
			Severity: model.SeverityWarning})
	}
	// Comms-loss: the server should send status every lease/2. Missing it for
	// longer than that (plus slack) is a momentary interruption — keep
	// outputting captions until the lease actually expires (§5.2 #2).
	if m.state == model.StatePlaying && !m.commsLost {
		silence := now.Sub(m.lastRequest)
		threshold := time.Duration(m.leaseMS/2+m.leaseMS/10) * time.Millisecond
		if silence > threshold {
			m.commsLost = true
			m.ctrl.CommsLost()
			m.log.Warn(logging.EvACSPDisconnected, logging.Field("reason", "comms_loss_continue_until_lease"))
			m.emitState()
		}
	}
}

// selectLanguages returns every available language in D8 priority order.
func (m *Machine) selectLanguages(avail []string) []string {
	if len(avail) == 0 {
		return nil
	}
	availSet := map[string]bool{}
	for _, l := range avail {
		availSet[l] = true
	}
	var out []string
	seen := map[string]bool{}
	// 1) venue/show-configured preference order (§5.4 / D8).
	for _, l := range m.prefLng {
		if availSet[l] && !seen[l] {
			out = append(out, l)
			seen[l] = true
		}
	}
	// 2) then any remaining available languages in RPL order.
	for _, l := range avail {
		if !seen[l] {
			out = append(out, l)
			seen[l] = true
		}
	}
	return out
}

// eagerLanguages returns the head of the priority order that is fetched up front —
// the patron-selected/venue-default languages first (§5.3). Anything beyond the cap
// is fetched on demand by RequestLanguage.
func (m *Machine) eagerLanguages(priority []string) []string {
	if m.eagerN <= 0 || len(priority) <= m.eagerN {
		return priority
	}
	out := make([]string, m.eagerN)
	copy(out, priority[:m.eagerN])
	return out
}

// RequestLanguage fetches a language that was not in the eager set and adds it to
// the active window, leaving the languages already rendering untouched (§5.3
// "switches to a language not yet fetched"). It is the hook the patron fan-out
// calls on a language switch. It returns false when the language is not offered by
// the current RPL; an already-loaded language is a no-op returning true.
//
// The brief "loading captions…" state §5.3 asks for is patron-facing and per
// language; the fan-out status is a single shared value, so publishing it here
// would degrade every other patron's page. The state belongs in the per-subscriber
// fan-out, driven by this call's return.
func (m *Machine) RequestLanguage(lang string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if lang == "" || m.activeSeg < 0 {
		return false
	}
	if !contains(m.availLangs, lang) {
		return false
	}
	if !contains(m.reqLangs, lang) {
		m.reqLangs = append(m.reqLangs, lang)
	}
	if contains(m.loadedLangs, lang) {
		return true
	}
	m.loadedLangs = append(m.loadedLangs, lang)
	m.loadReels()
	m.log.Info(logging.EvAssetFetched,
		logging.Field("language", lang),
		logging.Field("reason", "on_demand_language_switch"))
	return true
}

// LoadedLanguages returns the languages whose assets are currently fetched.
func (m *Machine) LoadedLanguages() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.loadedLangs))
	copy(out, m.loadedLangs)
	return out
}

// RetainedRPLCount returns how many RPL timeline segments are held. Bounded by
// maxRetainedSegments so an abandoned RPL cannot accumulate (§5.2 new-show-load).
func (m *Machine) RetainedRPLCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.segments)
}

// CachedContentBytes returns the size of the transient timed-text cache. Zero after
// a show ends (guardrail #5).
func (m *Machine) CachedContentBytes() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loader.CachedBytes()
}

// State returns the observable caption-link state (§10.3). Carries no caption text.
func (m *Machine) State() model.CaptionLinkState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

func (m *Machine) stateLocked() model.CaptionLinkState {
	now := m.clock()
	var remaining int64
	if m.leaseActive {
		remaining = m.leaseDeadline.Sub(now).Milliseconds()
		if remaining < 0 {
			remaining = 0
		}
	}
	curLang := ""
	if len(m.languages) > 0 {
		curLang = m.languages[0]
	}
	return model.CaptionLinkState{
		State:              m.state,
		Server:             m.server,
		LeaseActive:        m.leaseActive,
		LeaseExpiresInMS:   remaining,
		RequestIntervalMS:  m.leaseMS / 2,
		TimelinePositionMS: m.timelineMS,
		OutputMode:         m.outputMode,
		CurrentPlayoutID:   m.playoutID,
		RPLLoaded:          m.currentRPL != nil,
		AssetsLoaded:       len(m.loadedLangs) > 0,
		CurrentLanguage:    curLang,
		AvailableLanguages: m.availLangs,
		DriftMSEstimate:    m.ctrl.MaxDriftMS(),
	}
}

func (m *Machine) emitState() {
	if m.onState != nil {
		m.onState(m.stateLocked())
	}
}

func (m *Machine) warn(w model.Warning) {
	if m.onWarn != nil {
		m.onWarn(w)
	}
}

// CommsLost reports whether the machine is currently in a momentary comms-loss
// (continuing to output until lease expiry).
func (m *Machine) CommsLost() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commsLost
}

// SetSelfHealHook installs the callback fired when the lease genuinely expires, so
// a supervisor can re-run the connection sequence (§5.2 item 6). Passing nil
// clears it.
func (m *Machine) SetSelfHealHook(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onHeal = fn
}

// ResetForReconnect drops the link-scoped state before a re-dial: the lease, the
// show's content, and the timeline anchor. The controller is blanked, because a
// link that has to be rebuilt is not a momentary blip (§5.2 item 2 covers that).
func (m *Machine) ResetForReconnect() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaseActive = false
	m.commsLost = false
	m.leaseWarned = false
	m.timelineMS = 0
	m.playState = ""
	m.outputMode = model.OutputUnknown
	m.purgeContent()
	if m.ctrl != nil {
		m.ctrl.Stop()
		m.ctrl.LoadReel(map[string][]model.Cue{})
	}
	m.state = model.StateDisconnected
	m.emitState()
}

// Run drives the machine over a Transport in production: a reader goroutine feeds
// requests; a ticker performs the time-based checks. It returns when ctx is done
// or the transport dies; the caller (see Supervisor) re-dials and re-runs the
// connection sequence.
func (m *Machine) Run(ctx context.Context, t Transport) error {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
	m.Connected()
	defer func() {
		m.mu.Lock()
		m.runCtx = nil
		m.mu.Unlock()
	}()
	reqCh := make(chan Request, 8)
	errCh := make(chan error, 1)
	go func() {
		for {
			r, err := t.Recv(ctx)
			if err != nil {
				errCh <- err
				return
			}
			select {
			case reqCh <- r:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-reqCh:
			resp := m.Handle(req)
			_ = t.Send(ctx, resp)
		case <-ticker.C:
			m.Tick()
		case err := <-errCh:
			// Transport died: keep outputting until the lease expires (the ticker
			// path will fire LeaseExpired). Return so the caller can re-dial.
			return err
		}
	}
}
