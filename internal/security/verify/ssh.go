package verify

// Measuring whether SSH is actually a way into this appliance (§9.1 "SSH
// disabled by default after first setup", §9.3 "SSH state correct for the
// profile").
//
// WHAT THIS REPLACES, and why it was the sharpest item in the audit. The §9.3
// row an operator reads as "SSH disabled" was derived from a configuration
// SETTING — `ssh_enabled` — that nothing in the tree ever wrote. Absent, it read
// false, `checkSSH` returned correct, and the dashboard printed "SSH disabled:
// yes". On an appliance where sshd was genuinely running, the row said the
// opposite of the truth. That is fail-WRONG on a security row, which is the one
// thing this codebase treats as unacceptable, and it is the same defect the
// firewall row already had fixed (D17: measured, never asserted).
//
// WHAT IS MEASURED. A listening socket, not a unit file and not a process name.
// The question §9.1 is really asking is "is there a way in", and the answer is
// whether something is accepting connections on the SSH port — a disabled unit
// with a running daemon is still a way in, and an enabled unit that failed to
// start is not. /proc/net/tcp and /proc/net/tcp6 are readable without privilege,
// so this works on the appliance AND here.
//
// WHAT IS DISTINGUISHED. Listening on loopback only is not the same as listening
// on a network: §9.2 permits local SSH in the Lab profile, and an appliance
// reachable only from its own console is not the re-entry path §9.4 is about.

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// DefaultSSHPort is the port measured when none is configured.
const DefaultSSHPort = 22

// tcpListenState is the value /proc/net/tcp uses for TCP_LISTEN.
const tcpListenState = "0A"

// SSHStatus is what was observed about SSH on this host.
type SSHStatus struct {
	// Listening: something is accepting SSH connections somewhere.
	Listening bool
	// NetworkReachable: at least one of those listeners is on an address other
	// than loopback, i.e. SSH is a way in from a network rather than only from
	// this machine's own console.
	NetworkReachable bool
	// Port is the port that was measured.
	Port int
}

// SSHReader observes SSH's listening state. It is a seam so a test can supply an
// observation and so the production reader stays one small, replaceable thing.
type SSHReader func() (SSHStatus, error)

// StaticSSHReader returns a reader that always yields the given observation.
func StaticSSHReader(s SSHStatus, err error) SSHReader {
	return func() (SSHStatus, error) { return s, err }
}

// ProcSSHReader reads the kernel's TCP tables for listeners on port.
//
// It reports an ERROR rather than "not listening" when the tables cannot be
// read. The distinction is the whole point: "we looked and SSH is off" and "we
// could not look" lead an operator to do completely different things, and only
// one of them is a reason to open a house.
func ProcSSHReader(port int) SSHReader {
	if port <= 0 {
		port = DefaultSSHPort
	}
	return func() (SSHStatus, error) {
		status := SSHStatus{Port: port}
		var read bool
		for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			listening, reachable, err := scanListeners(path, port)
			if err != nil {
				continue
			}
			read = true
			status.Listening = status.Listening || listening
			status.NetworkReachable = status.NetworkReachable || reachable
		}
		if !read {
			return SSHStatus{Port: port}, errors.New(
				"neither /proc/net/tcp nor /proc/net/tcp6 could be read, so SSH's state is unknown")
		}
		return status, nil
	}
}

// scanListeners reports whether anything is listening on port in one table, and
// whether any such listener is bound to something other than loopback.
func scanListeners(path string, port int) (listening, networkReachable bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return false, false, err
	}
	defer func() { _ = f.Close() }()

	want := fmt.Sprintf("%04X", port)
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 || fields[3] != tcpListenState {
			continue
		}
		addr, p, ok := strings.Cut(fields[1], ":")
		if !ok || !strings.EqualFold(p, want) {
			continue
		}
		listening = true
		if !isLoopbackHex(addr) {
			networkReachable = true
		}
	}
	if err := sc.Err(); err != nil {
		return false, false, err
	}
	return listening, networkReachable, nil
}

// isLoopbackHex reports whether a /proc hex address is a loopback or wildcard-
// excluded local address.
//
// The kernel writes IPv4 little-endian, so 127.0.0.1 is "0100007F"; IPv6 ::1 is
// the 32-hex-digit form ending in 01000000. A WILDCARD bind (0.0.0.0 or ::) is
// deliberately NOT treated as loopback: it accepts from every interface, which
// is the most network-reachable case there is.
func isLoopbackHex(addr string) bool {
	switch len(addr) {
	case 8: // IPv4
		v, err := strconv.ParseUint(addr, 16, 32)
		if err != nil {
			return false
		}
		// Little-endian: the first octet of the address is the low byte.
		return byte(v) == 127
	case 32: // IPv6
		return strings.EqualFold(addr, "00000000000000000000000001000000")
	default:
		return false
	}
}
