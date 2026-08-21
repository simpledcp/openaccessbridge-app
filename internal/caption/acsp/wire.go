// Package acsp implements the ST 430-10 Auxiliary Content Synchronization Protocol
// client/state machine — modeled as a lease-and-timeline state machine, NOT a
// parser (buildplan.md §5.2).
//
// PROVISIONAL: request names/sequences and behaviors (Announce, Get New Lease,
// Update Timeline, Set RPL Location, Set Output Mode, Terminate Lease, periodic
// status; "request interval = half the lease"; "continue output on comms loss
// until lease expiry") are taken from public ISDCF implementation notes (§5.1),
// not the licensed standard. The wire types and Transport here are a clearly
// marked seam: the real Phase 0A wire contract (framing, ports, TCP-initiation
// direction) is swapped in without changing the Machine. The default initiation
// direction is Variant A (ACS-initiated) — DECISIONS D6.
package acsp

import (
	"context"
	"errors"
	"io"
)

// RequestType is an ACSP request issued by the server (media block) to the ACS
// (OpenAccessBridge). The media block drives the application-layer exchange; the ACS
// responds (§5.1).
type RequestType string

const (
	ReqAnnounce       RequestType = "announce"
	ReqGetNewLease    RequestType = "get_new_lease"
	ReqUpdateTimeline RequestType = "update_timeline"
	ReqSetRPLLocation RequestType = "set_rpl_location"
	ReqSetOutputMode  RequestType = "set_output_mode"
	ReqTerminateLease RequestType = "terminate_lease"
	ReqStatus         RequestType = "status" // periodic keepalive at half-lease cadence
)

// PlayState mirrors the server's transport state in an Update Timeline request.
type PlayState string

const (
	PlayPlaying PlayState = "playing"
	PlayPaused  PlayState = "paused"
	PlayStopped PlayState = "stopped"
)

// Request is a server→ACS ACSP request (PROVISIONAL fields).
type Request struct {
	Type RequestType `json:"type"`

	// Announce
	ServerID     string `json:"server_id,omitempty"`
	ServerFamily string `json:"server_family,omitempty"`
	ScreenName   string `json:"screen_name,omitempty"`
	Fingerprint  string `json:"fingerprint,omitempty"`

	// GetNewLease / Status
	LeaseMS int64 `json:"lease_ms,omitempty"`

	// UpdateTimeline
	TimelineMS int64     `json:"timeline_ms,omitempty"`
	PlayoutID  string    `json:"playout_id,omitempty"`
	PlayState  PlayState `json:"play_state,omitempty"`

	// SetRPLLocation: a locator the ACS fetches, or inline RPL bytes (simulator).
	RPLLocation string `json:"rpl_location,omitempty"`
	RPLInline   []byte `json:"rpl_inline,omitempty"`

	// SetOutputMode
	OutputEnabled *bool `json:"output_enabled,omitempty"`
}

// Response is the ACS→server reply.
type Response struct {
	Type   RequestType `json:"type"`
	OK     bool        `json:"ok"`
	State  string      `json:"state,omitempty"`
	Reason string      `json:"reason,omitempty"`
}

// Transport is the ACS side of the ACSP connection: it receives requests from the
// server and sends responses. Implementations: ChanTransport (in-process, used by
// the simulator) and (Phase 0A) a TCP transport with the real framing codec.
type Transport interface {
	Recv(ctx context.Context) (Request, error)
	Send(ctx context.Context, r Response) error
	Close() error
}

// ErrTransportClosed indicates the underlying link is gone (comms loss / TCP drop).
var ErrTransportClosed = errors.New("acsp transport closed")

// ChanTransport is the ACS side of an in-process channel link.
type ChanTransport struct {
	in     <-chan Request
	out    chan<- Response
	closed chan struct{}
}

// ServerLink is the simulator (server) side of an in-process channel link.
type ServerLink struct {
	out    chan<- Request
	in     <-chan Response
	closed chan struct{}
}

// NewChanPair builds a connected (ACS, server) pair over buffered channels.
func NewChanPair(buf int) (*ChanTransport, *ServerLink) {
	reqCh := make(chan Request, buf)
	respCh := make(chan Response, buf)
	closed := make(chan struct{})
	acs := &ChanTransport{in: reqCh, out: respCh, closed: closed}
	srv := &ServerLink{out: reqCh, in: respCh, closed: closed}
	return acs, srv
}

func (c *ChanTransport) Recv(ctx context.Context) (Request, error) {
	select {
	case r, ok := <-c.in:
		if !ok {
			return Request{}, io.EOF
		}
		return r, nil
	case <-c.closed:
		return Request{}, ErrTransportClosed
	case <-ctx.Done():
		return Request{}, ctx.Err()
	}
}

func (c *ChanTransport) Send(ctx context.Context, r Response) error {
	select {
	case c.out <- r:
		return nil
	case <-c.closed:
		return ErrTransportClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *ChanTransport) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// Send delivers a request from the server to the ACS.
func (s *ServerLink) Send(ctx context.Context, r Request) error {
	select {
	case s.out <- r:
		return nil
	case <-s.closed:
		return ErrTransportClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Recv reads the ACS's response.
func (s *ServerLink) Recv(ctx context.Context) (Response, error) {
	select {
	case r, ok := <-s.in:
		if !ok {
			return Response{}, io.EOF
		}
		return r, nil
	case <-s.closed:
		return Response{}, ErrTransportClosed
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Drop simulates a hard communications loss (TCP drop) by closing the link.
func (s *ServerLink) Drop() {
	select {
	case <-s.closed:
	default:
		close(s.closed)
	}
}

// BoolPtr is a helper for the SetOutputMode OutputEnabled field.
func BoolPtr(b bool) *bool { return &b }
