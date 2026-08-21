package main

// The patron-network scope this process applies to its WebRTC peer (§9.1, §6.7,
// DECISIONS D27). Core reads the configured client interface out of the config
// store and pushes it here, because only this process holds the peer that does
// the gathering.

import (
	"encoding/json"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/audio/webrtcpeer"
	"github.com/simpledcp/openaccessbridge/internal/ipc"
	"github.com/simpledcp/openaccessbridge/internal/logging"
)

func (m *manager) handleNetwork(msg ipc.Message) *ipc.Message {
	var scope ipc.MediaNetworkScope
	if err := json.Unmarshal(msg.Payload, &scope); err != nil {
		return networkResult(msg, ipc.MediaNetworkResult{Error: "malformed patron network scope"})
	}
	name := strings.TrimSpace(scope.PatronInterface)
	if m.peers == nil {
		// There is no peer to scope. Reporting "applied" here would tell the
		// dashboard that patron traffic is confined on an appliance that cannot
		// serve a patron at all.
		return networkResult(msg, ipc.MediaNetworkResult{
			Error: "no webrtc peer in this process: nothing to confine",
		})
	}
	if err := m.peers.SetPatronScope(webrtcpeer.PatronScope{Interface: name}); err != nil {
		// A named refusal, not a silent fallback: the appliance keeps whatever
		// scope it had and core raises it on the dashboard (§8.3).
		m.log.Error(logging.EvProcessUnhealthy,
			logging.Field("phase", "webrtc_patron_scope_refused"),
			logging.Field("interface", name),
			logging.Field("error", err.Error()))
		return networkResult(msg, ipc.MediaNetworkResult{Error: err.Error(), Report: m.peers.ScopeReport()})
	}
	return networkResult(msg, ipc.MediaNetworkResult{Applied: true, Interface: name, Report: m.peers.ScopeReport()})
}

func networkResult(m ipc.Message, r ipc.MediaNetworkResult) *ipc.Message {
	payload, _ := json.Marshal(r)
	return &ipc.Message{Type: m.Type, ID: m.ID, Payload: payload}
}
