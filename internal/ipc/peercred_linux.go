//go:build linux

package ipc

import (
	"errors"
	"net"
	"syscall"
)

// peerCredSupported reports whether this platform can identify the connecting
// process. Where it cannot, an AllowPeer policy fails CLOSED (§3.5: the
// privileged helper must not accept commands it cannot attribute).
const peerCredSupported = true

// peerOf returns the connecting process's credentials via SO_PEERCRED. The
// kernel stamps these at connect(2) time, so they cannot be forged by the peer.
//
// Note SO_PEERCRED reports the peer's PRIMARY gid only — supplementary groups
// are not visible here. A peer authorized by group must therefore run with the
// shared group as its primary group (the systemd units set Group=captionbridge).
func peerOf(conn net.Conn) (Peer, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Peer{}, errors.New("ipc: not a unix connection")
	}
	rc, err := uc.SyscallConn()
	if err != nil {
		return Peer{}, err
	}
	var (
		cred *syscall.Ucred
		serr error
	)
	if err := rc.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return Peer{}, err
	}
	if serr != nil {
		return Peer{}, serr
	}
	return Peer{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}, nil
}
