package main

// The appliance half of §6.7's signaling. captionbridge-core relays a patron's
// SDP offer and ICE candidates here over the loopback socket (ipc.TypeMediaSignal)
// and this process — which owns the encoders — answers as the WebRTC peer.
//
// §9.4 IS ENFORCED HERE BY OMISSION: nothing in this file passes an SDP body, an
// ICE candidate or a peer id to the logger. What is logged is the frame type, the
// named stream and an error class. The peer state transitions themselves are
// logged by internal/audio/webrtcpeer under the webrtc.peer.* taxonomy.

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/simpledcp/captionbridge/internal/audio/webrtcpeer"
	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
)

// signalTimeout bounds one signaling exchange. Core's loopback client waits 5 s
// for a reply, so answering must finish inside that or the patron sees a relay
// failure with no explanation on either side.
const signalTimeout = 4 * time.Second

// handleSignal runs one relayed signaling frame against the peer hub.
func (m *manager) handleSignal(msg ipc.Message) *ipc.Message {
	var req ipc.SignalRequest
	if err := json.Unmarshal(msg.Payload, &req); err != nil {
		return signalReply(msg, ipc.SignalResponse{
			Type: ipc.SignalUnavailable, Reason: ipc.ReasonNegotiationFailed,
		})
	}
	if m.peers == nil {
		// No WebRTC peer in this process (it was built without one): say so as a
		// class rather than answering an offer nothing can serve.
		return signalReply(msg, ipc.SignalResponse{
			Type: ipc.SignalUnavailable, Stream: req.Stream, Reason: ipc.ReasonNoEncoder,
		})
	}
	switch req.Type {
	case ipc.SignalOffer:
		return signalReply(msg, m.answerOffer(req))
	case ipc.SignalICE:
		// A candidate for a peer that has gone away is not an error worth
		// reporting to a patron: the peer is already closed.
		_ = m.peers.AddCandidate(req.PeerID, webrtc.ICECandidateInit{
			Candidate:     req.Candidate,
			SDPMid:        strPtr(req.SDPMid),
			SDPMLineIndex: uint16Ptr(req.SDPMLineIndex),
		})
		return signalReply(msg, ipc.SignalResponse{Type: ipc.SignalICE, Stream: req.Stream})
	case ipc.SignalClose:
		m.peers.ClosePeer(req.PeerID)
		return signalReply(msg, ipc.SignalResponse{Type: ipc.SignalClose, Stream: req.Stream})
	default:
		return signalReply(msg, ipc.SignalResponse{
			Type: ipc.SignalUnavailable, Stream: req.Stream, Reason: ipc.ReasonNegotiationFailed,
		})
	}
}

// answerOffer negotiates one subscriber onto an already-running per-stream
// encoder. It never starts an encoder: a stream that is not being encoded is a
// refusal, not a reason to spin one up for one patron (§6.7 topology).
func (m *manager) answerOffer(req ipc.SignalRequest) ipc.SignalResponse {
	switch req.Stream {
	case model.StreamHI, model.StreamVIN:
	default:
		return ipc.SignalResponse{Type: ipc.SignalUnavailable, Stream: req.Stream, Reason: ipc.ReasonUnknownStream}
	}
	ctx, cancel := context.WithTimeout(m.parent, signalTimeout)
	defer cancel()

	sdp, err := m.peers.Answer(ctx, req.PeerID, req.Stream, req.SDP, req.MaxPeers)
	if err != nil {
		reason := ipc.ReasonNegotiationFailed
		switch {
		case errors.Is(err, webrtcpeer.ErrNoSource):
			reason = ipc.ReasonNoEncoder
		case errors.Is(err, webrtcpeer.ErrAtCapacity):
			reason = ipc.ReasonAtCapacity
		}
		// The class, never the offer: an SDP body carries the patron's device
		// addresses and fingerprints (§9.4).
		m.log.Warn(logging.EvWebRTCPeerClosed,
			logging.Field("stream", string(req.Stream)),
			logging.Field("reason", reason))
		return ipc.SignalResponse{Type: ipc.SignalUnavailable, Stream: req.Stream, Reason: reason}
	}
	return ipc.SignalResponse{Type: ipc.SignalAnswer, Stream: req.Stream, SDP: sdp}
}

func signalReply(msg ipc.Message, resp ipc.SignalResponse) *ipc.Message {
	payload, _ := json.Marshal(resp)
	return &ipc.Message{Type: msg.Type, ID: msg.ID, Payload: payload}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func uint16Ptr(i int) *uint16 {
	if i < 0 || i > 65535 {
		return nil
	}
	v := uint16(i)
	return &v
}
