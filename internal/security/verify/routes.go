package verify

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// route is one kernel routing-table entry as read from /proc/net. Only the fields
// the §9.3 assertions need are kept: which interface carries it and what it covers.
type route struct {
	Iface string
	Dest  *net.IPNet
}

// prefixLen returns the route's prefix length (0 for a default route).
func (r route) prefixLen() int {
	ones, _ := r.Dest.Mask.Size()
	return ones
}

// isDefault reports whether this is a default route (::/0 or 0.0.0.0/0) — the
// route that makes an interface's reachable scope unbounded.
func (r route) isDefault() bool { return r.prefixLen() == 0 }

// readRoutes parses the IPv4 and IPv6 routing tables under procNet. Both files
// exist on any Linux kernel with the matching family enabled; a missing IPv6
// table means IPv6 is off (the secure state, D17), while a missing IPv4 table is
// inconclusive and fails loud.
func readRoutes(procNet string) ([]route, error) {
	v4, err := readRoutesV4(filepath.Join(procNet, "route"))
	if err != nil {
		return nil, err
	}
	v6, err := readRoutesV6(filepath.Join(procNet, "ipv6_route"))
	if err != nil {
		return nil, err
	}
	return append(v4, v6...), nil
}

// readRoutesV4 parses /proc/net/route. Destination and Mask are printed as the
// hex of the little-endian 32-bit value, so 0101A8C0 is 192.168.1.1.
func readRoutesV4(path string) ([]route, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("the IPv4 routing table %s is missing", path)
		}
		return nil, fmt.Errorf("the IPv4 routing table could not be read: %w", err)
	}
	defer f.Close()

	var out []route
	sc := bufio.NewScanner(f)
	first := true
	for sc.Scan() {
		if first { // header row
			first = false
			continue
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		dest, err := hexLEIPv4(fields[1])
		if err != nil {
			continue
		}
		mask, err := hexLEIPv4(fields[7])
		if err != nil {
			continue
		}
		out = append(out, route{
			Iface: fields[0],
			Dest:  &net.IPNet{IP: dest.Mask(net.IPMask(mask.To4())), Mask: net.IPMask(mask.To4())},
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("the IPv4 routing table could not be read: %w", err)
	}
	return out, nil
}

// readRoutesV6 parses /proc/net/ipv6_route: destination address (32 hex chars),
// destination prefix length (hex), then source, next hop, metrics, and finally
// the interface name.
func readRoutesV6(path string) ([]route, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			// IPv6 disabled at the kernel: there is no v6 route to leak (D17).
			return nil, nil
		}
		return nil, fmt.Errorf("the IPv6 routing table could not be read: %w", err)
	}
	defer f.Close()

	var out []route
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		dest, err := hexIPv6(fields[0])
		if err != nil {
			continue
		}
		plen, err := strconv.ParseInt(fields[1], 16, 32)
		if err != nil || plen < 0 || plen > 128 {
			continue
		}
		mask := net.CIDRMask(int(plen), 128)
		out = append(out, route{
			Iface: fields[len(fields)-1],
			Dest:  &net.IPNet{IP: dest.Mask(mask), Mask: mask},
		})
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("the IPv6 routing table could not be read: %w", err)
	}
	return out, nil
}

// hexLEIPv4 decodes a /proc/net/route address field (little-endian hex u32).
func hexLEIPv4(s string) (net.IP, error) {
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return nil, err
	}
	return net.IPv4(byte(v), byte(v>>8), byte(v>>16), byte(v>>24)), nil
}

// hexIPv6 decodes a /proc/net/ipv6_route address field (32 hex chars, big-endian).
func hexIPv6(s string) (net.IP, error) {
	if len(s) != 32 {
		return nil, errors.New("not a 32-character IPv6 hex address")
	}
	ip := make(net.IP, 16)
	for i := 0; i < 16; i++ {
		b, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return nil, err
		}
		ip[i] = byte(b)
	}
	return ip, nil
}

// bestRoute returns the longest-prefix route covering ip, or false when nothing
// covers it (nothing routes there — the appliance genuinely cannot reach it).
func bestRoute(routes []route, ip net.IP) (route, bool) {
	best := route{}
	found := false
	for _, r := range routes {
		if r.Dest == nil || !sameFamily(r.Dest.IP, ip) || !r.Dest.Contains(ip) {
			continue
		}
		if !found || r.prefixLen() > best.prefixLen() {
			best, found = r, true
		}
	}
	return best, found
}

func sameFamily(a, b net.IP) bool { return (a.To4() != nil) == (b.To4() != nil) }

// readNameservers parses the nameserver lines of a resolv.conf. A missing file
// means no configured resolver, which is the secure state for an offline booth
// appliance — not an error.
func readNameservers(path string) ([]net.IP, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("the resolver configuration could not be read: %w", err)
	}
	defer f.Close()

	var out []net.IP
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		// Strip an IPv6 zone ("fe80::1%eth0") before parsing.
		addr := fields[1]
		if i := strings.IndexByte(addr, '%'); i > 0 {
			addr = addr[:i]
		}
		if ip := net.ParseIP(addr); ip != nil {
			out = append(out, ip)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("the resolver configuration could not be read: %w", err)
	}
	return out, nil
}

// routeFindings is the outcome of the route/DNS half of the verification pass
// (§9.3 bullets 3 and 4). Every field is derived from the appliance's OWN
// routing table and resolver config — nothing is probed on the network.
type routeFindings struct {
	// checked is false when the pass had nothing to assert against (no zone
	// interfaces configured yet). The report then claims nothing, rather than
	// claiming a pass it did not earn.
	checked bool
	// zonesSeparated: the patron side has no route that reaches the theater side.
	zonesSeparated bool
	// theaterScopeAllowlistOnly: the theater interface carries no route beyond the
	// configured allowlist (in particular, no default route).
	theaterScopeAllowlistOnly bool
	// scopeChecked is false when no allowlist is configured, so the scope
	// assertion could not be made.
	scopeChecked bool
	// dnsChecked is false when the pass never got as far as the resolver config
	// (e.g. the zones are already known to be joined), so no DNS claim is made.
	dnsChecked bool
	// dnsContained: no configured nameserver resolves out of the theater zone.
	dnsContained bool
	// dnsViaDefaultRoute: a nameserver is only reachable through a default route,
	// i.e. an internet-facing resolver.
	dnsViaDefaultRoute bool
	// dnsUnreadable is true when the resolver configuration could not be read, so
	// the DNS claim is UNKNOWN rather than known-bad. Both are loud; only the
	// message differs, and telling an operator "DNS crosses a zone boundary" when
	// we merely failed to open the file sends them after the wrong fault.
	dnsUnreadable bool
	// reasons carries short, content-free explanations for the warning messages.
	sharedReason  string
	scopeReason   string
	dnsReason     string
	unreadable    string
	theaterIface  string
	clientIface   string
	allowlistSize int
}

// checkRoutes performs the route and DNS half of §9.3 by introspection only:
//
//	§9.3: "theater-side reachable scope == allowlist only — verified primarily by
//	introspecting CaptionBridge's own routes and firewall permissions (the
//	appliance has no configured route or rule beyond the allowlist), *not* by
//	scanning the theater network"
//	§9.3: "no DNS/route leak across interfaces (the patron side cannot reach the
//	theater side or the internet through the appliance)"
//
// It never sends a packet. When the zone interfaces are not configured yet
// (fresh appliance, mid-setup) it reports checked=false and asserts nothing.
func checkRoutes(theaterIface, clientIface string, allowlist []string, p Paths) routeFindings {
	f := routeFindings{theaterIface: theaterIface, clientIface: clientIface}
	if theaterIface == "" || clientIface == "" {
		return f
	}
	f.checked = true
	f.zonesSeparated = true
	f.dnsContained = true
	f.theaterScopeAllowlistOnly = true

	// The two zones are never joined (§9.1) — the same NIC cannot serve both.
	if theaterIface == clientIface {
		f.zonesSeparated = false
		f.sharedReason = "the theater and patron zones are configured on the same interface (" + theaterIface + ")"
		return f
	}

	routes, err := readRoutes(p.ProcNet)
	if err != nil {
		// The posture is unknown, which is a loud failure, not a pass.
		f.unreadable = err.Error()
		f.zonesSeparated = false
		f.dnsChecked, f.dnsContained = true, false
		f.theaterScopeAllowlistOnly = false
		return f
	}

	var allowIPs []net.IP
	for _, a := range allowlist {
		if ip := net.ParseIP(strings.TrimSpace(a)); ip != nil {
			allowIPs = append(allowIPs, ip)
		}
	}
	f.allowlistSize = len(allowIPs)

	// (a) No allowlisted theater host may be reachable out of the patron
	//     interface: that is the patron side reaching the theater side.
	if reason, joined := patronPathToTheater(routes, clientIface, allowIPs); joined {
		f.zonesSeparated = false
		f.sharedReason = reason
	}
	// (b) No subnet may appear on both interfaces: overlapping on-link scopes are
	//     the two zones joined by another name.
	if f.zonesSeparated {
		if pfx, ok := overlappingPrefix(routes, theaterIface, clientIface); ok {
			f.zonesSeparated = false
			f.sharedReason = "the theater and patron interfaces both carry a route for " + pfx
		}
	}

	// (c) Theater-side scope == allowlist only. A default route on the theater
	//     interface makes the scope unbounded; any other route there must cover an
	//     allowlisted address to be justified.
	if len(allowIPs) > 0 {
		f.scopeChecked = true
		for _, r := range routes {
			if r.Iface != theaterIface || r.Dest == nil {
				continue
			}
			if r.isDefault() {
				f.theaterScopeAllowlistOnly = false
				f.scopeReason = "the theater interface " + theaterIface + " carries a default route, so its reachable scope is not the allowlist"
				break
			}
			if isLocalScope(r.Dest.IP) {
				continue // link-local / multicast: not a reachable-scope claim
			}
			covers := false
			for _, ip := range allowIPs {
				if sameFamily(r.Dest.IP, ip) && r.Dest.Contains(ip) {
					covers = true
					break
				}
			}
			if !covers {
				f.theaterScopeAllowlistOnly = false
				f.scopeReason = "the theater interface " + theaterIface + " carries a route (" + r.Dest.String() + ") that covers no allowlisted address"
				break
			}
		}
	}

	// (d) DNS must not leak across EITHER zone boundary. A resolver reached out of
	//     the theater interface puts query traffic into the cinema LAN; a resolver
	//     that lives ON the patron subnet puts it into a local-only network the
	//     appliance itself serves, where no legitimate resolver exists. A resolver
	//     reachable only through a default route is an internet-facing resolver
	//     (§9.4 "no internet dependence") — loud, but deliberately not blocking.
	f.dnsChecked = true
	ns, err := readNameservers(p.ResolvConf)
	if err != nil {
		// The resolver config could not be READ — the posture is unknown, not
		// known-bad. dnsUnreadable makes Run say so instead of sending the
		// operator after a leak that may not exist.
		f.dnsContained = false
		f.dnsUnreadable = true
		f.dnsReason = err.Error()
		return f
	}
	for _, s := range ns {
		r, ok := bestRoute(routes, s)
		if !ok {
			continue // nothing routes there: the resolver is unreachable, not a leak
		}
		if r.Iface == "lo" {
			continue // a local stub resolver stays on the appliance
		}
		if r.Iface == theaterIface && !inAllowlist(allowIPs, s) {
			f.dnsContained = false
			f.dnsReason = "a configured nameserver is reachable only across the theater interface " + theaterIface
			break
		}
		if r.isDefault() {
			// Reached through a default route: that is the internet-facing-resolver
			// case, not "a resolver lives on that subnet", so it must not be judged
			// as one regardless of which interface carries the default.
			f.dnsViaDefaultRoute = true
			continue
		}
		if r.Iface == clientIface {
			f.dnsContained = false
			f.dnsReason = "a configured nameserver is reachable only across the patron interface " + clientIface
			break
		}
	}
	return f
}

// patronPathToTheater reports whether the patron interface has ANY path to an
// allowlisted theater host (§9.3 "the patron side cannot reach the theater side").
//
// Two separate conditions, because longest-prefix matching hides one behind the
// other:
//
//   - the WINNING route to an allowlisted host leaves via the patron interface —
//     the appliance would put theater traffic into the patron zone right now;
//   - the patron interface carries any non-default route COVERING an allowlisted
//     host, even when a more specific theater route currently wins. That is still
//     the two zones joined: the patron NIC is configured to reach theater space,
//     and a metric change, a link flap, or the theater route going away promotes
//     it silently. Checking only the winner fails open on exactly that setup
//     (patron 10.10.20.0/24 + theater 10.10.20.0/25 reported "separated").
//
// Default routes are excluded from the second condition — a default route covers
// every address, so it would flag every deployment that has one; it is already
// caught by the first condition whenever it is the route that actually wins.
func patronPathToTheater(routes []route, clientIface string, allow []net.IP) (string, bool) {
	for _, ip := range allow {
		if r, ok := bestRoute(routes, ip); ok && r.Iface == clientIface {
			return "an allowlisted theater host routes out of the patron interface " + clientIface, true
		}
	}
	for _, r := range routes {
		if r.Iface != clientIface || r.Dest == nil || r.isDefault() || isLocalScope(r.Dest.IP) {
			continue
		}
		for _, ip := range allow {
			if sameFamily(r.Dest.IP, ip) && r.Dest.Contains(ip) {
				return "the patron interface " + clientIface + " carries a route (" + r.Dest.String() +
					") that covers an allowlisted theater host", true
			}
		}
	}
	return "", false
}

// inAllowlist reports whether ip is one of the explicitly allowlisted addresses.
func inAllowlist(allow []net.IP, ip net.IP) bool {
	for _, a := range allow {
		if a.Equal(ip) {
			return true
		}
	}
	return false
}

// overlappingPrefix reports a routed prefix that appears on both interfaces.
func overlappingPrefix(routes []route, a, b string) (string, bool) {
	onA := map[string]bool{}
	for _, r := range routes {
		if r.Iface == a && r.Dest != nil && !r.isDefault() && !isLocalScope(r.Dest.IP) {
			onA[r.Dest.String()] = true
		}
	}
	for _, r := range routes {
		if r.Iface == b && r.Dest != nil && onA[r.Dest.String()] {
			return r.Dest.String(), true
		}
	}
	return "", false
}

// isLocalScope reports whether a prefix is link-local or multicast — scopes that
// exist on every interface and say nothing about reachable theater scope.
func isLocalScope(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsLoopback()
}
