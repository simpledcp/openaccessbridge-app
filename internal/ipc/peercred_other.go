//go:build !linux

package ipc

import (
	"errors"
	"net"
)

// peerCredSupported is false off Linux: there is no SO_PEERCRED equivalent we
// rely on, so an AllowPeer policy fails CLOSED rather than admitting an
// unattributable caller. The appliance targets Linux (§4); this file exists so
// the package still builds for local tooling on other hosts.
const peerCredSupported = false

func peerOf(net.Conn) (Peer, error) {
	return Peer{}, errors.New("ipc: peer credentials unavailable on this platform")
}
