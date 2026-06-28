package acsp

import (
	"context"
	"time"

	"github.com/simpledcp/captionbridge/internal/caption"
	"github.com/simpledcp/captionbridge/internal/caption/resolver"
	"github.com/simpledcp/captionbridge/internal/caption/rpl"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// Options configures a Machine.
type Options struct {
	Controller          *caption.Controller
	Resolver            resolver.Resolver // fetches the RPL document
	Loader              *resolver.Loader  // fetches timed-text per reel
	Clock               func() time.Time
	Log                 *logging.Logger
	ExpectedFingerprint string   // wrong-room guard (§8.5); empty disables it
	PreferredLanguages  []string // default-language priority (§5.4 / D8)
	// WarnFraction: emit a lease-expiring warning when remaining < WarnFraction of
	// the lease. Default 0.25.
	WarnFraction float64
	OnState      func(model.CaptionLinkState)
	OnWarning    func(model.Warning)
	// FetchAheadReels overrides the loader's window when (re)loading reels.
	FetchAheadReels int
}

// Machine is the ST 430-10 ACS state machine. The Handle/Tick API is synchronous
// and clock-injectable so the scenario suite is deterministic and CI-able; Run
// wraps them in a production loop over a Transport.
type Machine struct {
	ctrl    *caption.Controller
	res     resolver.Resolver
	loader  *resolver.Loader
	clock   func() time.Time
	log     *logging.Logger
	expFP   string
	prefLng []string
	warnFr  float64
	onState func(model.CaptionLinkState)
	onWarn  func(model.Warning)
	window  int

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
	languages     []string
	availLangs    []string
	rplByPlayout  map[string]*rpl.RPL
	currentRPL    *rpl.RPL
	commsLost     bool
	leaseWarned   bool
	identityOK    bool
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
	return &Machine{
		ctrl:         opts.Controller,
		res:          opts.Resolver,
		loader:       opts.Loader,
		clock:        clk,
		log:          log,
		expFP:        opts.ExpectedFingerprint,
		prefLng:      opts.PreferredLanguages,
		warnFr:       wf,
		onState:      opts.OnState,
		onWarn:       opts.OnWarning,
		window:       opts.FetchAheadReels,
		state:        model.StateDisconnected,
		outputMode:   model.OutputUnknown,
		rplByPlayout: make(map[string]*rpl.RPL),
		identityOK:   true,
	}
}

// Connected marks the TCP connection established (Variant A: the ACS dialed the
// server). The server then drives the exchange.
func (m *Machine) Connected() {
	m.state = model.StateConnected
	m.lastRequest = m.clock()
	m.log.Info(logging.EvACSPConnected)
	m.emitState()
}

// Handle processes one server→ACS request and returns the ACS response. It is the
// single place protocol requests mutate state and drive the caption controller.
func (m *Machine) Handle(req Request) Response {
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
	// Wrong-room / misconfiguration guard (§8.5). A mismatch is surfaced loudly
	// but is not necessarily a cryptographic authentication (§10.5).
	if m.expFP != "" && req.Fingerprint != "" && req.Fingerprint != m.expFP {
		m.identityOK = false
		m.log.Warn(logging.EvServerIdentityMismatch, logging.Field("reason", "fingerprint_mismatch"))
		if m.onWarn != nil {
			m.onWarn(model.Warning{Code: model.WarnServerIdentityMismatch,
				Message:  "Connected cinema server does not match this auditorium's pinned identity.",
				Severity: model.SeverityCritical})
		}
	} else {
		m.identityOK = true
	}
	m.emitState()
	return Response{Type: ReqAnnounce, OK: true, State: string(m.state)}
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
	if m.leaseActive {
		ms := req.LeaseMS
		if ms <= 0 {
			ms = m.leaseMS
		}
		m.leaseDeadline = now.Add(time.Duration(ms) * time.Millisecond)
		m.leaseWarned = false
	}
	return Response{Type: ReqStatus, OK: true, State: string(m.state)}
}

func (m *Machine) handleUpdateTimeline(req Request) Response {
	prevPlayout := m.playoutID
	m.timelineMS = req.TimelineMS
	m.playState = req.PlayState
	if req.PlayoutID != "" {
		m.playoutID = req.PlayoutID
	}

	// Playout-ID quirk (§5.2 item 4): a repeated or zero Playout ID with an
	// advancing timeline is NOT a loop-back / seek-to-start. Disambiguate by
	// timeline position, never by Playout ID alone. We therefore drive the
	// controller purely from the timeline value and play state.
	_ = prevPlayout

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
	m.emitState()
	return Response{Type: ReqUpdateTimeline, OK: true, State: string(m.state)}
}

// advanceToPlaying moves to PLAYING once RPL+output allow it.
func (m *Machine) advanceToPlaying() {
	if m.outputMode == model.OutputDisabled {
		m.state = model.StatePlaying // playing transport, but captions suppressed
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
		data, err = m.res.Resolve(context.Background(), req.RPLLocation)
	}
	if err != nil || len(data) == 0 {
		m.log.Warn(logging.EvAssetFetchFailed, logging.Field("reason", "rpl_fetch_failed"))
		return Response{Type: ReqSetRPLLocation, OK: false, Reason: "rpl_fetch_failed"}
	}
	parsed, err := rpl.ParseBytes(data)
	if err != nil {
		m.log.Warn(logging.EvAssetFetchFailed, logging.Field("reason", "rpl_parse_failed"))
		return Response{Type: ReqSetRPLLocation, OK: false, Reason: "rpl_parse_failed"}
	}
	// Key by Playout ID, but never rely on it for loop-back detection (§5.2 #4).
	key := parsed.PlayoutID
	m.rplByPlayout[key] = parsed
	m.currentRPL = parsed
	m.availLangs = parsed.Languages()
	m.languages = m.selectLanguages(m.availLangs)
	m.log.Info(logging.EvRPLReceived,
		logging.Field("playout_id_present", key != ""),
		logging.Field("reel_count", len(parsed.Reels)),
		logging.Field("language_count", len(m.availLangs)))

	m.loadReels(0)
	if m.state == model.StateTimelineActive || m.state == model.StateLeased {
		m.state = model.StateRPLReceived
	}
	if m.state == model.StateRPLReceived {
		m.state = model.StateReady
	}
	m.emitState()
	return Response{Type: ReqSetRPLLocation, OK: true, State: string(m.state)}
}

// loadReels (re)loads the fetch-ahead window into the controller for the selected
// languages. An empty RPL (no caption resources) yields no cues — "no captions for
// this segment," not an error (§5.2 item 5).
func (m *Machine) loadReels(currentReelIdx int) {
	if m.currentRPL == nil || m.loader == nil {
		return
	}
	cues, warns, err := m.loader.LoadWindow(context.Background(), m.currentRPL, currentReelIdx, m.languages)
	if err != nil {
		return
	}
	for range warns {
		m.log.Warn(logging.EvAssetFetchFailed, logging.Field("reason", "reel_asset_unavailable"))
	}
	m.ctrl.LoadReel(cues)
}

func (m *Machine) handleSetOutputMode(req Request) Response {
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
		m.ctrl.SetPatronStatus(model.PatronCaptionNoCaptions)
	case model.OutputEnabled:
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
	m.currentRPL = nil
	m.rplByPlayout = make(map[string]*rpl.RPL)
	m.availLangs = nil
	m.languages = nil
	m.playoutID = ""
	m.ctrl.Stop()
	m.ctrl.LoadReel(map[string][]model.Cue{}) // retire all language engines
	m.state = model.StateShowChange
	m.log.Info(logging.EvStateTransition, logging.Field("phase", "show_change"))
	m.emitState()
	return Response{Type: ReqTerminateLease, OK: true, State: string(m.state)}
}

// Tick performs time-based checks: lease expiry, lease-expiring warning, and
// comms-loss detection. Production calls it on a ticker; tests call it after
// advancing the injected clock.
func (m *Machine) Tick() {
	now := m.clock()
	if m.leaseActive {
		remaining := m.leaseDeadline.Sub(now)
		if remaining <= 0 {
			// Lease genuinely expired → blank captions and self-heal (§5.2 #6).
			m.leaseActive = false
			m.state = model.StateLeaseExpired
			m.ctrl.LeaseExpired()
			m.log.Warn(logging.EvLeaseExpired)
			m.emitState()
			return
		}
		if !m.leaseWarned && remaining < time.Duration(float64(m.leaseMS)*m.warnFr)*time.Millisecond {
			m.leaseWarned = true
			m.log.Warn(logging.EvLeaseExpiring, logging.Field("remaining_ms", remaining.Milliseconds()))
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
}

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

// State returns the observable caption-link state (§10.3). Carries no caption text.
func (m *Machine) State() model.CaptionLinkState {
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
		AssetsLoaded:       len(m.languages) > 0,
		CurrentLanguage:    curLang,
		AvailableLanguages: m.availLangs,
		DriftMSEstimate:    m.ctrl.MaxDriftMS(),
	}
}

func (m *Machine) emitState() {
	if m.onState != nil {
		m.onState(m.State())
	}
}

// CommsLost reports whether the machine is currently in a momentary comms-loss
// (continuing to output until lease expiry).
func (m *Machine) CommsLost() bool { return m.commsLost }

// Run drives the machine over a Transport in production: a reader goroutine feeds
// requests; a ticker performs the time-based checks. It returns when ctx is done
// or the transport dies; the lease/comms state persists so a caller can re-dial
// and resume (lease-expiry self-heal, §5.2 #6).
func (m *Machine) Run(ctx context.Context, t Transport) error {
	m.Connected()
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
