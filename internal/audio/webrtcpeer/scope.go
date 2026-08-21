package webrtcpeer

// ICE gathering scope (buildplan.md §6.7 local-only, §9.1 interface separation,
// DECISIONS D27's "Known limit").
//
// D27 settled that the appliance answers with gathering COMPLETE, and noted what
// it did not fix: pion enumerates EVERY non-loopback interface, so the answer
// handed to a patron's phone carried a host candidate for each one. On a
// multi-homed appliance that means the phone is told to try the venue's docker
// bridges, the operator's VPN, and every IPv6 privacy address the patron NIC
// happens to be rotating through.
//
// Two costs, neither of them cosmetic:
//
//  1. JOIN TIME, at the worst possible moment. ICE tries the pairs it is given.
//     Candidates on networks the phone cannot route to do not fail instantly —
//     they fail on a timeout, and they fail for every phone in the room at once,
//     because an auditorium's patrons all scan the code in the same 90 seconds.
//     §6.7 spends its whole budget on milliseconds; handing out unreachable
//     candidates spends seconds.
//  2. It describes the venue's private topology to anything on the patron LAN.
//     No packet leaves the LAN (there is still no STUN and no TURN), but an SDP
//     answer that names an overlay subnet is a fingerprint the appliance had no
//     reason to publish.
//
// So gathering is confined to the interface a patron can actually reach — the
// §9.1 client interface, which the setup wizard already records
// (config.SettingClientInterface) and which core pushes down to this process.
//
// The honesty rule this file exists to keep: a scope that was CONFIGURED is not
// the same fact as a scope that is IN FORCE, and neither is the same fact as the
// addresses a patron was really offered. Only the last one is a measurement, so
// only the last one is what Report exposes to the dashboard — read back out of
// the SDP this hub actually emitted, not out of the intent that produced it.

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// ErrPatronInterfaceUnusable is returned when a patron scope names an interface
// this host cannot gather on — it does not exist, it is down, or it holds no
// address. Answering anyway would produce an SDP with no host candidate in it,
// which a phone cannot tell apart from a network it cannot reach: it would spin
// until ICE gave up and the patron would be told nothing. Refusing names the
// fault while the booth can still fix it (guardrail #7, no silent degradation).
var ErrPatronInterfaceUnusable = errors.New("webrtcpeer: the configured patron interface cannot be gathered on")

// PatronScope confines ICE gathering to the network a patron's phone is on.
//
// The zero value gathers on every interface, which is what the appliance did
// before this existed. That default is deliberate and it is NOT silent: an
// unconfigured scope is reported as unconfined, and the addresses it hands out
// are measured and surfaced, so "we never scoped this" can never read on a
// dashboard as "we scoped this and it was fine".
type PatronScope struct {
	// Interface is the §9.1 client-interface NAME (e.g. "wlan0"), as recorded by
	// the setup wizard. Empty means no scope is configured.
	//
	// A name rather than an address: an appliance's patron NIC can renumber (DHCP
	// lease, IPv6 privacy rotation) without becoming a different NIC, and the
	// question ICE has to answer is "which wire", not "which address today".
	Interface string
}

// Configured reports whether an operator has actually chosen a patron interface.
func (s PatronScope) Configured() bool { return strings.TrimSpace(s.Interface) != "" }

// name is the trimmed interface name.
func (s PatronScope) name() string { return strings.TrimSpace(s.Interface) }

// allowInterface is the predicate pion's SettingEngine consults for every
// interface it is about to gather on.
func (s PatronScope) allowInterface(name string) bool {
	if !s.Configured() {
		return true
	}
	return name == s.name()
}

// usable checks that the configured interface exists, is up, and holds at least
// one address that could carry media. It is checked when a scope is set AND
// again when an answer is built, because the two are separated by a whole show:
// a USB Wi-Fi adapter pulled out mid-performance takes the patron interface with
// it, and the next patron to join must be refused by name rather than handed an
// answer with nothing in it.
func (s PatronScope) usable() error {
	if !s.Configured() {
		return nil
	}
	iface, err := net.InterfaceByName(s.name())
	if err != nil {
		return fmt.Errorf("%w: %q is not an interface on this host", ErrPatronInterfaceUnusable, s.name())
	}
	if iface.Flags&net.FlagUp == 0 {
		return fmt.Errorf("%w: %q is down", ErrPatronInterfaceUnusable, s.name())
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return fmt.Errorf("%w: %q addresses unreadable: %v", ErrPatronInterfaceUnusable, s.name(), err)
	}
	for _, a := range addrs {
		ip := addrIP(a)
		// Loopback and unspecified addresses are not a patron path, and pion
		// excludes loopback from gathering anyway — an interface holding nothing
		// else is an interface no phone can reach.
		if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return nil
		}
	}
	return fmt.Errorf("%w: %q holds no address a patron could reach", ErrPatronInterfaceUnusable, s.name())
}

func addrIP(a net.Addr) net.IP {
	switch v := a.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

// interfaceOfAddr maps a candidate address back to the interface holding it, so
// the report can name what was offered instead of listing raw addresses (which
// on a patron NIC are a rotating set of IPv6 privacy addresses, and are a client
// identifier's neighbours — §9.4 keeps them off the dashboard and out of logs).
func interfaceOfAddr(ip net.IP) string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if got := addrIP(a); got != nil && got.Equal(ip) {
				return iface.Name
			}
		}
	}
	return ""
}

// candidateAddrs extracts the host-candidate addresses from an SDP body. It
// reads the SDP the hub EMITTED, which is the only artifact that can answer
// "what was this phone actually told to try".
func candidateAddrs(sdp string) []net.IP {
	var out []net.IP
	for _, line := range strings.Split(sdp, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		idx := strings.Index(line, "candidate:")
		if idx < 0 {
			continue
		}
		// candidate:<foundation> <component> <transport> <priority> <addr> <port> typ <type> …
		fields := strings.Fields(line[idx:])
		if len(fields) < 6 {
			continue
		}
		ip := net.ParseIP(fields[4])
		if ip == nil {
			continue
		}
		out = append(out, ip)
	}
	return out
}
