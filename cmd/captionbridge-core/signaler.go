package main

// The production web.AudioSignaler (§6.7). §6.7 puts signaling on "the existing
// local web server's WebSocket — no third-party signaling", which makes this
// process the RELAY and captionbridge-media the peer: a patron's offer arrives
// on the patron socket (internal/web/audio.go), crosses one loopback Unix socket
// as ipc.TypeMediaSignal, and the answer comes straight back.
//
// It is a relay in the strict sense — it moves opaque bodies and looks at none
// of them. §9.4 forbids recording WebRTC SDP bodies or client identifiers, so
// nothing here logs the offer, the answer, a candidate or the peer id; what is
// logged is the frame type and an error class.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/simpledcp/captionbridge/internal/ipc"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/web"
)

// mediaSignaler relays one patron's signaling frames to the media helper.
type mediaSignaler struct {
	media *mediaClient
	log   *logging.Logger
	// maxPeers is the deployment profile's concurrent-client cap (D11, §9.5).
	// The profile is this process's to know and the peers are the media
	// process's to hold, so the cap travels with each offer.
	maxPeers int
}

// Signal implements web.AudioSignaler.
func (s *mediaSignaler) Signal(ctx context.Context, peerID string, in web.SignalFrame) ([]web.SignalFrame, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if peerID == "" {
		// internal/web could not mint an identity, so this peer could not be
		// told apart from another patron's. Refusing is the safe direction.
		return nil, web.ErrPlaybackRefused{Class: ipc.ReasonNegotiationFailed}
	}
	req := ipc.SignalRequest{
		PeerID: peerID, Type: in.Type, Stream: in.Stream,
		SDP: in.SDP, Candidate: in.Candidate,
		SDPMid: in.SDPMid, SDPMLineIndex: in.SDPMLineIndex,
	}
	if in.Type == ipc.SignalOffer {
		req.MaxPeers = s.maxPeers
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := s.media.request(ipc.Message{Type: ipc.TypeMediaSignal, ID: "signal", Payload: payload})
	if err != nil {
		// The helper is not answering. That is a media-process fault, not a
		// patron's: record it the same way every other failed loopback request
		// is recorded, so the dashboard shows the process as down.
		s.media.noteDown(err.Error())
		return nil, err
	}
	var out ipc.SignalResponse
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		return nil, errors.New("malformed signaling response")
	}
	switch out.Type {
	case ipc.SignalAnswer:
		return []web.SignalFrame{{Type: ipc.SignalAnswer, Stream: out.Stream, SDP: out.SDP}}, nil
	case ipc.SignalUnavailable:
		// The CLASS goes back; internal/web owns the sentence a patron reads.
		return nil, web.ErrPlaybackRefused{Class: out.Reason}
	default:
		// ICE and close are acknowledged with nothing to send back. The
		// appliance does not trickle candidates of its own: with no STUN or TURN
		// its host candidates are gathered before the answer is returned, so the
		// answer already carries them.
		return nil, nil
	}
}
