package ipc

// The patron-network scope core pushes to openaccessbridge-media (buildplan.md
// §9.1 interface separation, §6.7 local-only, DECISIONS D27).
//
// Core owns the configuration store, so it is the only process that knows which
// interface the setup wizard designated as the patron/client side. Media owns
// the WebRTC peer, so it is the only process that can confine ICE gathering to
// that interface. This message is the seam between those two facts.
//
// It is deliberately a separate message from media.configure: that one carries
// the §10.5 auditorium AUDIO profile (device, channel mapping), and an appliance
// whose patron NIC changed has not changed which jack its HI feed arrives on.
// Folding the two together would mean a network correction restarted the room's
// audio.

import "github.com/simpledcp/openaccessbridge/internal/model"

const (
	// TypeMediaNetwork — core → helper: the patron-facing interface ICE
	// gathering must be confined to. The reply is a MediaNetworkResult, which
	// reports whether the scope was ACCEPTED — an interface the media process
	// cannot gather on is refused with a reason rather than stored and quietly
	// serving nobody.
	TypeMediaNetwork = "media.network"
)

// MediaNetworkScope is the request payload: which interface a patron's phone can
// reach this appliance on.
//
// An empty PatronInterface means "no scope configured", which is a real state on
// an appliance whose setup wizard has not been run — not an error, and not
// something to be filled in with a guess. It gathers on every interface, and the
// dashboard says so.
type MediaNetworkScope struct {
	// PatronInterface is config.SettingClientInterface: an interface NAME, since
	// a patron NIC keeps its identity across a DHCP renumber.
	PatronInterface string `json:"patron_interface"`
}

// MediaNetworkResult is the reply. Applied is a fact about the media process's
// peer, not an acknowledgement that the message arrived: core surfaces a refusal
// on the dashboard, because an appliance advertising the wrong network to phones
// is a fault an operator has to see (§8.3 no silent degradation).
type MediaNetworkResult struct {
	Applied bool `json:"applied"`
	// Interface echoes the scope now in force (empty when unconfined).
	Interface string `json:"interface,omitempty"`
	// Error is a reason class plus the offending name, never a patron-facing
	// sentence.
	Error string `json:"error,omitempty"`
	// Report is the peer's own account of its gathering — including which
	// interfaces the last patron was really offered. Core polls this rather than
	// remembering what it pushed: a media process that restarted came back
	// unconfined, and an appliance that believes its own last write would keep
	// reporting a scope that is no longer in force.
	Report model.ICEScopeReport `json:"report"`
}
