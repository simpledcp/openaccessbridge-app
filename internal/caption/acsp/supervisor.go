package acsp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/model"
)

// Connector opens ONE ACSP link and returns the transport for it. Variant A (the
// D6 default) dials the allowlisted cinema server; Variant B waits for the server
// to connect and MUST reject any peer other than the allowlisted address — the
// firewall enforces the same rule at the packet level (§9.1), and the two are
// belt-and-braces, not alternatives.
//
// The real connector is Phase 0A work (BLOCKER B1): the framing/PDU codec, the
// port, and the initiation direction are all confirmed against a live server
// before a TCP implementation is written. Everything above this seam — dialing
// policy, backoff, reconnect, lease-expiry self-heal — is finished and tested here.
type Connector interface {
	Connect(ctx context.Context, addr string) (Transport, error)
}

// ConnectorFunc adapts a function to Connector.
type ConnectorFunc func(ctx context.Context, addr string) (Transport, error)

// Connect implements Connector.
func (f ConnectorFunc) Connect(ctx context.Context, addr string) (Transport, error) {
	return f(ctx, addr)
}

// SupervisorOptions configures a Supervisor.
type SupervisorOptions struct {
	Machine *Machine
	// ServerAddr is the allowlisted cinema-server address as host:port. The
	// supervisor never connects anywhere else.
	ServerAddr string
	// Initiation records which side opens the connection (§5.1 / D6). Variant A is
	// the default; Variant B is a config change, not a rewrite.
	Initiation model.ACSPInitiation
	Connector  Connector
	// MinBackoff/MaxBackoff bound the capped exponential retry. Defaults 1s / 30s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
	Log        *logging.Logger
	// Sleep is the delay seam (tests inject an immediate one). Defaults to a timer.
	Sleep func(ctx context.Context, d time.Duration)
}

// Supervisor keeps the ACSP link up. It owns exactly the behavior the state
// machine cannot own by itself: opening the connection (§5.1 "OpenAccessBridge (the
// ACS) opens the TCP connection"), re-opening it with capped backoff when it drops,
// and re-running the whole connection sequence after a lease expiry instead of
// leaving the ACS in a dead state (§5.2 item 6).
type Supervisor struct {
	m          *Machine
	addr       string
	initiation model.ACSPInitiation
	conn       Connector
	minBackoff time.Duration
	maxBackoff time.Duration
	log        *logging.Logger
	sleep      func(ctx context.Context, d time.Duration)
}

// NewSupervisor validates the options and builds a Supervisor.
func NewSupervisor(opts SupervisorOptions) (*Supervisor, error) {
	if opts.Machine == nil {
		return nil, errors.New("acsp supervisor: no machine")
	}
	if opts.Connector == nil {
		return nil, errors.New("acsp supervisor: no connector (Phase 0A wire codec — BLOCKER B1)")
	}
	if err := validateServerAddr(opts.ServerAddr); err != nil {
		return nil, err
	}
	init := opts.Initiation
	if init == "" {
		init = model.ACSInitiated // D6 default
	}
	minB := opts.MinBackoff
	if minB <= 0 {
		minB = time.Second
	}
	maxB := opts.MaxBackoff
	if maxB < minB {
		maxB = 30 * time.Second
	}
	log := opts.Log
	if log == nil {
		log = logging.Nop()
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}
	return &Supervisor{
		m: opts.Machine, addr: opts.ServerAddr, initiation: init, conn: opts.Connector,
		minBackoff: minB, maxBackoff: maxB, log: log, sleep: sleep,
	}, nil
}

// SupervisorFromProfile builds a Supervisor from the persisted auditorium profile:
// CinemaServerIP + ACSPPort give the allowlisted address and ACSPInitiation selects
// the variant (§5.1, §10.5, D6). ServerAddr/Initiation in opts are ignored.
func SupervisorFromProfile(p model.AuditoriumProfile, opts SupervisorOptions) (*Supervisor, error) {
	if p.CinemaServerIP == "" {
		return nil, errors.New("acsp supervisor: auditorium profile has no cinema server IP")
	}
	if p.ACSPPort <= 0 || p.ACSPPort > 65535 {
		return nil, fmt.Errorf("acsp supervisor: auditorium profile has no usable ACSP port (%d)", p.ACSPPort)
	}
	opts.ServerAddr = net.JoinHostPort(p.CinemaServerIP, strconv.Itoa(p.ACSPPort))
	opts.Initiation = p.ACSPInitiation
	return NewSupervisor(opts)
}

// validateServerAddr rejects anything that is not a concrete host:port. An empty or
// malformed address must fail loudly at construction rather than silently never
// connecting (guardrail: no silent degradation).
func validateServerAddr(addr string) error {
	if addr == "" {
		return errors.New("acsp supervisor: no cinema server address configured")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("acsp supervisor: bad server address: %w", err)
	}
	if host == "" {
		return errors.New("acsp supervisor: server address has no host")
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("acsp supervisor: bad ACSP port %q", portStr)
	}
	return nil
}

// Addr returns the single allowlisted address this supervisor will use.
func (s *Supervisor) Addr() string { return s.addr }

// Initiation returns the configured connection-initiation variant.
func (s *Supervisor) Initiation() model.ACSPInitiation { return s.initiation }

// Run keeps the link up until ctx is cancelled. Each pass opens one link, drives
// the machine over it, and — however that link ended, including a lease expiry that
// fired the self-heal hook — resets and re-runs the whole connection sequence after
// a capped backoff. It returns only ctx's error.
func (s *Supervisor) Run(ctx context.Context) error {
	backoff := s.minBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := time.Now()
		err := s.runOnce(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		s.m.ResetForReconnect()
		if time.Since(start) >= s.maxBackoff {
			backoff = s.minBackoff // the link was healthy for a while: start over
		}
		s.log.Warn(logging.EvACSPDisconnected,
			logging.Field("reason", classifyLinkEnd(err)),
			logging.Field("retry_in_ms", backoff.Milliseconds()),
			logging.Field("initiation", string(s.initiation)))
		s.sleep(ctx, backoff)
		backoff *= 2
		if backoff > s.maxBackoff {
			backoff = s.maxBackoff
		}
	}
}

// runOnce opens one link and drives the machine over it until it ends. A lease
// expiry cancels the link's context through the self-heal hook, so the reconnect
// path is the SAME path as a transport drop — one recovery route, not two.
func (s *Supervisor) runOnce(ctx context.Context) error {
	linkCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.m.SetSelfHealHook(cancel)
	defer s.m.SetSelfHealHook(nil)

	t, err := s.conn.Connect(linkCtx, s.addr)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer t.Close()
	s.log.Info(logging.EvACSPConnected,
		logging.Field("initiation", string(s.initiation)))
	return s.m.Run(linkCtx, t)
}

// classifyLinkEnd maps a link outcome to a loggable class — never a raw error
// string, which can carry an address or a path (§9.4).
func classifyLinkEnd(err error) string {
	switch {
	case err == nil:
		return "link_closed"
	case errors.Is(err, context.Canceled):
		return "link_reset_self_heal"
	case errors.Is(err, ErrTransportClosed):
		return "transport_closed"
	default:
		return "link_error"
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
