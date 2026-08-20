package ipc

// The WebRTC signaling relay's payloads (buildplan.md §6.7). §6.7 puts SDP
// offer/answer and ICE on "the existing local web server's WebSocket — no
// third-party signaling", which makes captionbridge-core the relay and
// captionbridge-media the peer. These are the two shapes that cross the loopback
// socket between them (TypeMediaSignal).
//
// WHAT MUST NEVER HAPPEN TO THESE FIELDS. SDP and Candidate are opaque bodies;
// PeerID is a client identifier. §9.4 forbids recording any of the three, so
// neither process logs them — the relay logs the frame TYPE, the named stream
// and error classes. They are carried, not observed.

import "github.com/simpledcp/captionbridge/internal/model"

// Signal frame types, matching the vocabulary the patron page already speaks
// (internal/web/assets/patron.js).
const (
	SignalOffer  = "offer"
	SignalICE    = "ice"
	SignalClose  = "close"
	SignalAnswer = "answer"
	// SignalUnavailable reports that no listening session could be established.
	// It carries an error CLASS, never a message written for a patron: the calm
	// patron copy is composed by the web layer (§7.2).
	SignalUnavailable = "unavailable"
)

// Reason classes on an unavailable response. They are named states, not
// sentences, so the patron-facing wording lives in one place (internal/web).
const (
	// ReasonNoEncoder: nothing is encoding this stream, so there is nothing to
	// subscribe to.
	ReasonNoEncoder = "no_encoder"
	// ReasonAtCapacity: the deployment profile's concurrent-client cap is full
	// (D11, §9.5).
	ReasonAtCapacity = "at_capacity"
	// ReasonNegotiationFailed: the peer could not be built from this offer.
	ReasonNegotiationFailed = "negotiation_failed"
	// ReasonUnknownStream: a stream name this appliance does not serve.
	ReasonUnknownStream = "unknown_stream"
)

// SignalRequest is one signaling frame relayed from a patron browser to the
// media process.
type SignalRequest struct {
	// PeerID identifies ONE browser subscriber for the life of its control
	// socket. It is minted by core per connection, is meaningless outside this
	// appliance, and is never logged (§9.4 "no client identifiers").
	PeerID string           `json:"peer_id"`
	Type   string           `json:"type"`
	Stream model.StreamType `json:"stream,omitempty"`
	// SDP is the browser's offer body (type "offer"). Opaque; never logged.
	SDP string `json:"sdp,omitempty"`
	// Candidate is one trickled ICE candidate (type "ice"). Opaque; never logged.
	Candidate     string `json:"candidate,omitempty"`
	SDPMid        string `json:"sdp_mid,omitempty"`
	SDPMLineIndex int    `json:"sdp_m_line_index,omitempty"`
	// MaxPeers is the deployment profile's concurrent-client cap (D11), passed
	// with each offer because the profile is core's to know and the peers are
	// media's to hold. 0 means unlimited.
	MaxPeers int `json:"max_peers,omitempty"`
}

// SignalResponse is what the media process sends back for one request.
type SignalResponse struct {
	Type   string           `json:"type"`
	Stream model.StreamType `json:"stream,omitempty"`
	// SDP is the appliance's answer, with host ICE candidates already gathered
	// (§6.7: no STUN/TURN, so gathering is local and immediate). Never logged.
	SDP string `json:"sdp,omitempty"`
	// Reason is one of the classes above when Type is unavailable.
	Reason string `json:"reason,omitempty"`
}
