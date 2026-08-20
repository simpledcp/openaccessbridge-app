package verify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// routeSpec is one fixture routing-table entry: a CIDR reachable out of iface.
type routeSpec struct {
	iface string
	cidr  string
}

// writeRouteFixture builds /proc/net/route and /proc/net/ipv6_route in the kernel's
// own on-disk encoding (v4 addresses are the hex of the little-endian u32; v6 are
// 32 big-endian hex chars plus a hex prefix length) plus a resolv.conf.
func writeRouteFixture(t *testing.T, procNet string, routes []routeSpec, nameservers ...string) string {
	t.Helper()
	mustMkdir(t, procNet)

	var v4, v6 strings.Builder
	v4.WriteString("Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n")
	for _, r := range routes {
		ip, ipnet, err := net.ParseCIDR(r.cidr)
		if err != nil {
			t.Fatalf("bad fixture CIDR %q: %v", r.cidr, err)
		}
		if ip.To4() != nil {
			ones, _ := ipnet.Mask.Size()
			fmt.Fprintf(&v4, "%s\t%s\t00000000\t0003\t0\t0\t100\t%s\t0\t0\t0\n",
				r.iface, hexLE(ipnet.IP.To4()), hexLE(net.CIDRMask(ones, 32)))
			continue
		}
		ones, _ := ipnet.Mask.Size()
		fmt.Fprintf(&v6, "%s %02x %s 00 %s 00000400 00000001 00000000 00000001 %s\n",
			hexBE(ipnet.IP.To16()), ones, strings.Repeat("0", 32), strings.Repeat("0", 32), r.iface)
	}
	mustWrite(t, filepath.Join(procNet, "route"), v4.String())
	mustWrite(t, filepath.Join(procNet, "ipv6_route"), v6.String())

	resolv := filepath.Join(procNet, "resolv.conf")
	var rc strings.Builder
	rc.WriteString("# fixture\n")
	for _, ns := range nameservers {
		rc.WriteString("nameserver " + ns + "\n")
	}
	mustWrite(t, resolv, rc.String())
	return resolv
}

// hexLE renders 4 address bytes the way /proc/net/route does.
func hexLE(b []byte) string {
	return fmt.Sprintf("%02X%02X%02X%02X", b[3], b[2], b[1], b[0])
}

// hexBE renders 16 address bytes the way /proc/net/ipv6_route does.
func hexBE(b []byte) string {
	var s strings.Builder
	for _, x := range b {
		fmt.Fprintf(&s, "%02x", x)
	}
	return s.String()
}

// routeCfg builds a Config whose kernel state is clean, so only the route/DNS
// findings under test can produce warnings.
func routeCfg(t *testing.T, procNet, resolv string, allowlist []string) Config {
	t.Helper()
	p := fixture(t, "0", "0", false)
	p.ProcNet = procNet
	p.ResolvConf = resolv
	return Config{
		Paths:            p,
		Profile:          model.ProfilePatronFacing,
		Ruleset:          parseReader(t, compliant()),
		TheaterIface:     "eth0",
		ClientIface:      "wlan0",
		TheaterAllowlist: allowlist,
	}
}

// TestRoutes_ZoneAndDNSAssertions covers §9.3's two introspection bullets that had
// no implementation: "theater-side reachable scope == allowlist only" and "no
// DNS/route leak across interfaces". Every case is pure file introspection — the
// spec forbids scanning the theater network, so introspection IS the requirement.
func TestRoutes_ZoneAndDNSAssertions(t *testing.T) {
	cases := []struct {
		name        string
		routes      []routeSpec
		nameservers []string
		allowlist   []string
		wantPass    bool
		wantCodes   []string
		wantZones   bool
		wantScope   bool
		wantDNS     bool
	}{
		{
			name: "separated zones, allowlist-only scope, local resolver",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"wlan0", "192.168.50.0/24"},
				{"lo", "127.0.0.0/8"},
			},
			nameservers: []string{"127.0.0.53"},
			allowlist:   []string{"10.10.20.15"},
			wantPass:    true, wantZones: true, wantScope: true, wantDNS: true,
		},
		{
			name: "default route on the theater interface widens the scope",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"eth0", "0.0.0.0/0"},
				{"wlan0", "192.168.50.0/24"},
			},
			allowlist: []string{"10.10.20.15"},
			wantPass:  false, wantZones: true, wantScope: false, wantDNS: true,
			wantCodes: []string{model.WarnDNSLeak},
		},
		{
			name: "a theater route covering nothing allowlisted",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"eth0", "172.16.0.0/16"},
				{"wlan0", "192.168.50.0/24"},
			},
			allowlist: []string{"10.10.20.15"},
			wantPass:  false, wantZones: true, wantScope: false, wantDNS: true,
			wantCodes: []string{model.WarnDNSLeak},
		},
		{
			name: "the allowlisted server routes out of the patron interface",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"wlan0", "10.10.20.0/24"},
			},
			allowlist: []string{"10.10.20.15"},
			wantPass:  false, wantZones: false, wantScope: true, wantDNS: true,
			wantCodes: []string{model.WarnSharedInterface},
		},
		{
			name: "a resolver reachable only across the theater interface",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"wlan0", "192.168.50.0/24"},
			},
			nameservers: []string{"10.10.20.53"},
			allowlist:   []string{"10.10.20.15"},
			wantPass:    false, wantZones: true, wantScope: true, wantDNS: false,
			wantCodes: []string{model.WarnDNSLeak},
		},
		{
			name: "an internet resolver is loud but not blocking",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"wlan0", "192.168.50.0/24"},
				{"wlan0", "0.0.0.0/0"},
			},
			nameservers: []string{"9.9.9.9"},
			allowlist:   []string{"10.10.20.15"},
			wantPass:    true, wantZones: true, wantScope: true, wantDNS: true,
			wantCodes: []string{model.WarnDNSLeak},
		},
		{
			name: "IPv6 theater route beyond the allowlist",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"eth0", "2001:db8:beef::/48"},
				{"wlan0", "192.168.50.0/24"},
			},
			allowlist: []string{"10.10.20.15"},
			wantPass:  false, wantZones: true, wantScope: false, wantDNS: true,
			wantCodes: []string{model.WarnDNSLeak},
		},
		{
			name: "link-local routes on the theater side are not a scope claim",
			routes: []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"eth0", "169.254.0.0/16"},
				{"eth0", "fe80::/64"},
				{"wlan0", "192.168.50.0/24"},
			},
			allowlist: []string{"10.10.20.15"},
			wantPass:  true, wantZones: true, wantScope: true, wantDNS: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			procNet := filepath.Join(t.TempDir(), "proc", "net")
			resolv := writeRouteFixture(t, procNet, tc.routes, tc.nameservers...)
			rep := Run(routeCfg(t, procNet, resolv, tc.allowlist))

			if !rep.RoutesIntrospected {
				t.Fatal("RoutesIntrospected should be true when both zone interfaces are known")
			}
			if rep.ZonesSeparated != tc.wantZones {
				t.Errorf("ZonesSeparated = %v, want %v", rep.ZonesSeparated, tc.wantZones)
			}
			if rep.TheaterScopeAllowlistOnly != tc.wantScope {
				t.Errorf("TheaterScopeAllowlistOnly = %v, want %v", rep.TheaterScopeAllowlistOnly, tc.wantScope)
			}
			if rep.DNSContained != tc.wantDNS {
				t.Errorf("DNSContained = %v, want %v", rep.DNSContained, tc.wantDNS)
			}
			if rep.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v (warnings: %v)", rep.Pass, tc.wantPass, rep.Warnings)
			}
			if got := codes(rep.Warnings); !equalStrings(got, tc.wantCodes) {
				t.Errorf("warning codes = %v, want %v", got, tc.wantCodes)
			}
		})
	}
}

// TestRoutes_SameInterfaceForBothZones: §9.1's "two zones, never joined" has no
// profile exception, so one NIC serving both is critical everywhere.
func TestRoutes_SameInterfaceForBothZones(t *testing.T) {
	for _, profile := range []model.FirewallProfile{
		model.ProfileLab, model.ProfileBoothDiagnostic, model.ProfilePatronFacing, model.ProfileLowCostReplacement,
	} {
		t.Run(string(profile), func(t *testing.T) {
			procNet := filepath.Join(t.TempDir(), "proc", "net")
			resolv := writeRouteFixture(t, procNet, []routeSpec{{"eth0", "10.10.20.0/24"}})
			cfg := routeCfg(t, procNet, resolv, []string{"10.10.20.15"})
			cfg.Profile = profile
			cfg.ClientIface = "eth0" // one NIC for both zones

			rep := Run(cfg)
			if rep.ZonesSeparated {
				t.Error("one interface for both zones must not report separated zones")
			}
			if rep.Pass {
				t.Error("a shared zone interface must fail the pass in every profile")
			}
			if !contains(codes(rep.Warnings), model.WarnSharedInterface) {
				t.Errorf("expected %s, got %v", model.WarnSharedInterface, codes(rep.Warnings))
			}
			// Joined zones short-circuit the rest of the route pass, so the report
			// must not claim the assertions it never reached.
			if rep.DNSContained || rep.TheaterScopeAllowlistOnly {
				t.Errorf("unreached assertions must not be reported as satisfied: %+v", rep)
			}
		})
	}
}

// TestRoutes_LabScopeIsNotAsserted: §9.2 grants Lab "broader discovery allowed",
// so a Lab bench must not be nagged about a theater route beyond the allowlist —
// while every other profile is allowlist-only.
func TestRoutes_ScopeSeverityByProfile(t *testing.T) {
	cases := []struct {
		profile      model.FirewallProfile
		wantCode     bool
		wantSeverity string
		wantPass     bool
	}{
		{model.ProfileLab, false, "", true},
		{model.ProfileBoothDiagnostic, true, model.SeverityWarning, true},
		{model.ProfilePatronFacing, true, model.SeverityCritical, false},
		{model.ProfileLowCostReplacement, true, model.SeverityCritical, false},
		{model.FirewallProfile("mystery"), true, model.SeverityCritical, false},
	}
	for _, tc := range cases {
		t.Run(string(tc.profile), func(t *testing.T) {
			procNet := filepath.Join(t.TempDir(), "proc", "net")
			resolv := writeRouteFixture(t, procNet, []routeSpec{
				{"eth0", "10.10.20.0/24"},
				{"eth0", "0.0.0.0/0"},
				{"wlan0", "192.168.50.0/24"},
			})
			cfg := routeCfg(t, procNet, resolv, []string{"10.10.20.15"})
			cfg.Profile = tc.profile

			rep := Run(cfg)
			var got *model.Warning
			for i := range rep.Warnings {
				if rep.Warnings[i].Code == model.WarnDNSLeak {
					got = &rep.Warnings[i]
				}
			}
			if tc.wantCode != (got != nil) {
				t.Fatalf("scope warning present = %v, want %v (warnings %v)", got != nil, tc.wantCode, rep.Warnings)
			}
			if got != nil && got.Severity != tc.wantSeverity {
				t.Errorf("severity = %q, want %q", got.Severity, tc.wantSeverity)
			}
			if rep.Pass != tc.wantPass {
				t.Errorf("Pass = %v, want %v", rep.Pass, tc.wantPass)
			}
		})
	}
}

// TestRoutes_SkippedBeforeInterfacesAreAssigned: mid-setup the zones are not
// assigned yet. The pass must then claim nothing rather than reporting a green
// route posture it never checked.
func TestRoutes_SkippedBeforeInterfacesAreAssigned(t *testing.T) {
	rep := Run(Config{
		Paths:   fixture(t, "0", "0", false),
		Profile: model.ProfilePatronFacing,
		Ruleset: parseReader(t, compliant()),
	})
	if rep.RoutesIntrospected || rep.ZonesSeparated || rep.TheaterScopeAllowlistOnly || rep.DNSContained {
		t.Errorf("unchecked route assertions must not be reported as satisfied: %+v", rep)
	}
	if !rep.Pass {
		t.Errorf("a mid-setup appliance with clean kernel state should still pass: %v", rep.Warnings)
	}
}

// TestRoutes_UnreadableTableFailsLoud: if the routing table cannot be read, the
// posture is unknown — which is a loud failure, never a silent pass.
func TestRoutes_UnreadableTableFailsLoud(t *testing.T) {
	dir := t.TempDir()
	cfg := routeCfg(t, filepath.Join(dir, "nonexistent"), filepath.Join(dir, "resolv.conf"), []string{"10.10.20.15"})
	rep := Run(cfg)
	if rep.Pass {
		t.Error("an unreadable routing table must fail the pass")
	}
	if !contains(codes(rep.Warnings), model.WarnSharedInterface) {
		t.Errorf("expected a zone-separation warning, got %v", codes(rep.Warnings))
	}
}

// TestReadRoutes_RealProcNet decodes this host's actual routing table. It asserts
// only self-consistency (the decoder handles the kernel's real encoding), never a
// particular network — the dev host is not an appliance.
func TestReadRoutes_RealProcNet(t *testing.T) {
	if _, err := os.Stat("/proc/net/route"); err != nil {
		t.Skip("no /proc/net/route on this host")
	}
	routes, err := readRoutes("/proc/net")
	if err != nil {
		t.Fatalf("readRoutes: %v", err)
	}
	if len(routes) == 0 {
		t.Fatal("expected at least one route on a networked host")
	}
	for _, r := range routes {
		if r.Iface == "" || r.Dest == nil {
			t.Fatalf("decoded a malformed route: %+v", r)
		}
		if ones, bits := r.Dest.Mask.Size(); bits == 0 || ones > bits {
			t.Fatalf("decoded a malformed mask for %s: %v", r.Iface, r.Dest)
		}
		// The stored destination must be the network address of its own prefix.
		if !r.Dest.IP.Equal(r.Dest.IP.Mask(r.Dest.Mask)) {
			t.Fatalf("route destination is not a network address: %v", r.Dest)
		}
	}
}

// TestHexDecoders pins the /proc encodings against known values.
func TestHexDecoders(t *testing.T) {
	ip, err := hexLEIPv4("0101A8C0")
	if err != nil || ip.String() != "192.168.1.1" {
		t.Errorf("hexLEIPv4(0101A8C0) = %v, %v; want 192.168.1.1", ip, err)
	}
	mask, err := hexLEIPv4("0000FFFF")
	if err != nil || mask.String() != "255.255.0.0" {
		t.Errorf("hexLEIPv4(0000FFFF) = %v, %v; want 255.255.0.0", mask, err)
	}
	v6, err := hexIPv6("fd7a115ca1e000000000000000000053")
	if err != nil || v6.String() != "fd7a:115c:a1e0::53" {
		t.Errorf("hexIPv6 = %v, %v; want fd7a:115c:a1e0::53", v6, err)
	}
	if _, err := hexIPv6("tooshort"); err == nil {
		t.Error("hexIPv6 should reject a short field")
	}
}
