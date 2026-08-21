package main

import (
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/ipc"
)

func pushScope(t *testing.T, m *manager, iface string) ipc.MediaNetworkResult {
	t.Helper()
	payload, err := json.Marshal(ipc.MediaNetworkScope{PatronInterface: iface})
	if err != nil {
		t.Fatal(err)
	}
	msg := m.handle(ipc.Message{Type: ipc.TypeMediaNetwork, ID: "net", Payload: payload})
	if msg == nil {
		t.Fatal("the media process did not answer media.network")
	}
	var res ipc.MediaNetworkResult
	if err := json.Unmarshal(msg.Payload, &res); err != nil {
		t.Fatalf("malformed media.network reply: %v", err)
	}
	return res
}

// firstRoutableInterface is the patron interface stand-in on the test host.
func firstRoutableInterface(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, aerr := i.Addrs()
		if aerr == nil && len(addrs) > 0 {
			return i.Name
		}
	}
	t.Skip("no non-loopback interface on this host")
	return ""
}

// Core owns the config store; this process owns the peer. The §9.1 client
// interface has to cross that seam before ICE gathering can be confined to it
// (D27).
func TestTheConfiguredPatronInterfaceIsAppliedToThePeer(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	iface := firstRoutableInterface(t)

	res := pushScope(t, m, iface)
	if !res.Applied || res.Error != "" {
		t.Fatalf("pushing the patron interface %q = applied:%v error:%q", iface, res.Applied, res.Error)
	}
	if !res.Report.InForce || res.Report.Interface != iface {
		t.Fatalf("the reply reports in_force:%v interface:%q", res.Report.InForce, res.Report.Interface)
	}
}

// An interface that is not there is a configuration fault and must come back as
// one. Storing it and carrying on would leave the appliance gathering on a
// network that does not exist — every patron refused, and a dashboard with
// nothing on it to explain why.
func TestAnUnusablePatronInterfaceIsRefusedWithAReason(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	res := pushScope(t, m, "cb-not-an-interface0")
	if res.Applied {
		t.Fatal("the media process applied a patron interface that does not exist")
	}
	if res.Error == "" {
		t.Fatal("a refused scope came back with no reason")
	}
	if res.Report.InForce {
		t.Fatalf("a refused scope is reported as in force: %+v", res.Report)
	}
}

// The empty scope is a real state — the setup wizard has not run — and it must
// be reported as UNCONFINED rather than as a scope that was applied. This is the
// green-looking-lie shape for this feature: the appliance gathers on every
// interface it has, and a dashboard says "confined".
func TestAnUnconfiguredScopeIsAppliedAsUnconfinedAndSaysSo(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	res := pushScope(t, m, "")
	if !res.Applied {
		t.Fatalf("an empty scope was refused: %q", res.Error)
	}
	if res.Report.Configured || res.Report.InForce {
		t.Fatalf("no patron interface was configured, but the report claims configured:%v in_force:%v",
			res.Report.Configured, res.Report.InForce)
	}
}

// A scope can be corrected without restarting the media process: an operator who
// mistyped the interface in the setup wizard fixes it and the next patron gets
// the corrected answer.
func TestAPatronInterfaceCanBeCorrectedWithoutARestart(t *testing.T) {
	m := newLoggedManager(t, &syncBuffer{})
	iface := firstRoutableInterface(t)

	if res := pushScope(t, m, "cb-not-an-interface0"); res.Applied {
		t.Fatal("a bad interface was accepted")
	}
	res := pushScope(t, m, iface)
	if !res.Applied || res.Report.Interface != iface {
		t.Fatalf("the corrected interface did not take: %+v", res)
	}
}

// §9.4: the media process must not log addresses. The refusal path names the
// interface (operational fact) and the reason class, and that is all.
//
// Both halves are asserted. An earlier version of this test checked only that
// SOMETHING was logged, which is the weaker half of its own name — it would have
// passed with every local address in the line.
func TestARefusedScopeLogsTheInterfaceNotAnAddress(t *testing.T) {
	logs := &syncBuffer{}
	m := newLoggedManager(t, logs)
	iface := firstRoutableInterface(t)

	// A successful scope first, so there are real addresses in play that COULD
	// have leaked, then the refusal this test is about.
	pushScope(t, m, iface)
	pushScope(t, m, "cb-not-an-interface0")
	out := logs.String()

	if !strings.Contains(out, "webrtc_patron_scope_refused") {
		t.Fatalf("a refused patron interface was not logged:\n%s", out)
	}
	if !strings.Contains(out, "cb-not-an-interface0") {
		t.Fatalf("the log does not name the interface that was refused:\n%s", out)
	}
	// The second half: no address of this host may appear anywhere in the log.
	for _, addr := range localAddrs(t) {
		if strings.Contains(out, addr) {
			t.Fatalf("the local address %s reached the log:\n%s", addr, out)
		}
	}
}

// localAddrs is every address this host holds, so the assertion is about real
// values rather than a pattern that might not match.
func localAddrs(t *testing.T) []string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range ifaces {
		addrs, err := i.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipnet, ok := a.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				out = append(out, ipnet.IP.String())
			}
		}
	}
	return out
}
