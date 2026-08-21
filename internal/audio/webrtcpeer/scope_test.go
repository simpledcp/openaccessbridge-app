package webrtcpeer

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

// gatherableIfaces returns the interfaces pion would gather host candidates on:
// up, not loopback, holding at least one address that can BECOME a candidate.
//
// It uses the package's own `gatherable` predicate deliberately. An earlier
// version of this helper had its own copy of the rule, which meant the helper
// could not expose a blind spot in the rule it was standing in for — and there
// was one: an interface holding only an IPv6 link-local address passed, and then
// gathered nothing (RFC 8445 §5.1.1.1).
func gatherableIfaces(t *testing.T) []string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate interfaces: %v", err)
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if gatherable(addrIP(a)) {
				out = append(out, iface.Name)
				break
			}
		}
	}
	return out
}

// answerOnce publishes a stream, joins as a browser would, and returns the SDP
// the appliance answered with.
func answerOnce(t *testing.T, h *Hub, peerID string) string {
	t.Helper()
	if !h.Publishing(model.StreamHI) {
		if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
			t.Fatal(err)
		}
	}
	b := newBrowser(t)
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	answer, err := h.Answer(context.Background(), peerID, model.StreamHI, offer.SDP, 0)
	if err != nil {
		t.Fatalf("answer: %v", err)
	}
	return answer
}

// interfacesIn names the interfaces whose addresses appear as host candidates in
// an SDP body — read out of the SDP, the same way the hub's own report is.
func interfacesIn(t *testing.T, sdp string) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, ip := range candidateAddrs(sdp) {
		name := interfaceOfAddr(ip)
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// The point of the whole file (§6.7, D27's known limit): with a patron interface
// configured, every host candidate the appliance hands a phone belongs to THAT
// interface. Before scoping existed this host answered with candidates on five
// interfaces — two VPN overlays, two docker bridges and the LAN NIC — and a
// phone would have spent its join time timing out against four of them.
func TestAnAnswerOffersOnlyThePatronInterface(t *testing.T) {
	ifaces := gatherableIfaces(t)
	if len(ifaces) == 0 {
		t.Skip("no gatherable interface on this host")
	}
	patron := ifaces[0]

	h := newHub(t)
	if err := h.SetPatronScope(PatronScope{Interface: patron}); err != nil {
		t.Fatalf("scope to %q: %v", patron, err)
	}
	answer := answerOnce(t, h, "peer")

	if !strings.Contains(answer, "typ host") {
		t.Fatal("the scoped answer carries no host candidate at all: a patron could not connect")
	}
	got := interfacesIn(t, answer)
	for _, name := range got {
		if name != patron {
			t.Fatalf("the answer offers a candidate on %q; the patron interface is %q", name, patron)
		}
	}
}

// An unconfigured scope keeps the old behaviour — and says so. The failure this
// guards against is a dashboard that reads "confined" because the code has a
// scoping feature, on an appliance where nobody ever configured one.
func TestAnUnscopedHubReportsThatItIsUnconfined(t *testing.T) {
	h := newHub(t)
	rep := h.ScopeReport()
	if rep.Configured || rep.InForce {
		t.Fatalf("a hub with no patron interface reports configured=%v in_force=%v", rep.Configured, rep.InForce)
	}
	if rep.Answered {
		t.Fatal("a hub that has answered nobody reports that it has answered")
	}

	answer := answerOnce(t, h, "peer")
	rep = h.ScopeReport()
	if !rep.Answered {
		t.Fatal("after answering a patron the report still says nothing was answered")
	}
	if got, want := len(rep.OfferedInterfaces), len(interfacesIn(t, answer)); got != want {
		t.Fatalf("report names %d offered interfaces, the answer carries %d", got, want)
	}
	if len(gatherableIfaces(t)) > 1 && len(rep.OfferedInterfaces) < 2 {
		t.Fatalf("this host has %d gatherable interfaces but the unscoped hub reported offering %v — "+
			"the report is describing intent, not the answer", len(gatherableIfaces(t)), rep.OfferedInterfaces)
	}
}

// The report must be a MEASUREMENT of the emitted SDP. If it were derived from
// the configured scope it would look identical on a working appliance and be a
// fabrication on a broken one — which is exactly the class of green-looking lie
// the §6.7 review pass caught twice.
func TestTheScopeReportMeasuresTheAnswerAndNotTheIntent(t *testing.T) {
	ifaces := gatherableIfaces(t)
	if len(ifaces) == 0 {
		t.Skip("no gatherable interface on this host")
	}
	h := newHub(t)
	if err := h.SetPatronScope(PatronScope{Interface: ifaces[0]}); err != nil {
		t.Fatal(err)
	}
	// Configured and in force, but nobody has joined: there is nothing measured
	// to report, and the honest answer is silence rather than the configured name.
	rep := h.ScopeReport()
	if !rep.Configured || !rep.InForce {
		t.Fatalf("configured scope reports configured=%v in_force=%v", rep.Configured, rep.InForce)
	}
	if rep.Answered || len(rep.OfferedInterfaces) != 0 || rep.OfferedCandidates != 0 {
		t.Fatalf("with no answer produced the report already claims offered=%v candidates=%d",
			rep.OfferedInterfaces, rep.OfferedCandidates)
	}

	answer := answerOnce(t, h, "peer")
	rep = h.ScopeReport()
	if want := len(candidateAddrs(answer)); rep.OfferedCandidates != want {
		t.Fatalf("report counts %d candidates, the answer carries %d", rep.OfferedCandidates, want)
	}
}

// A patron interface that is not there is a configuration fault, and it must be
// reported as one. Answering anyway produces an SDP with no host candidate,
// which a phone cannot distinguish from a dead network: the patron sees a
// spinner and the booth sees nothing at all.
func TestAMissingPatronInterfaceIsRefusedRatherThanAnsweredEmpty(t *testing.T) {
	h := newHub(t)
	err := h.SetPatronScope(PatronScope{Interface: "cb-not-an-interface0"})
	if !errors.Is(err, ErrPatronInterfaceUnusable) {
		t.Fatalf("setting a missing patron interface = %v, want ErrPatronInterfaceUnusable", err)
	}
	// The rejected scope must not have taken hold: the hub keeps gathering the
	// way it did before the bad push, rather than silently serving nobody.
	if rep := h.ScopeReport(); rep.Configured {
		t.Fatalf("a rejected scope was stored anyway: %+v", rep)
	}

	// A hub CONSTRUCTED with a bad scope (core pushed it at startup) refuses the
	// offer by name instead of answering with nothing in it.
	bad, err := New(Options{Scope: PatronScope{Interface: "cb-not-an-interface0"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bad.Close)
	if err := bad.Publish(model.StreamHI, newFakeEncoder()); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t)
	offer, err := b.pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.pc.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Answer(context.Background(), "peer", model.StreamHI, offer.SDP, 0); !errors.Is(err, ErrPatronInterfaceUnusable) {
		t.Fatalf("answer on a missing patron interface = %v, want ErrPatronInterfaceUnusable", err)
	}
	if rep := bad.ScopeReport(); rep.Unusable == "" || rep.InForce {
		t.Fatalf("a hub scoped to a missing interface reports unusable=%q in_force=%v", rep.Unusable, rep.InForce)
	}
}

// Core learns the patron interface from the setup wizard, which can happen after
// the media process has already started answering. The scope must therefore
// apply to the NEXT answer without rebuilding the peer API — otherwise the
// appliance would have to be restarted to stop advertising the operator's VPN.
func TestAScopePushedAfterStartupAppliesToTheNextAnswer(t *testing.T) {
	ifaces := gatherableIfaces(t)
	if len(ifaces) < 2 {
		t.Skip("this host has fewer than two gatherable interfaces: nothing to narrow")
	}
	h := newHub(t)
	before := interfacesIn(t, answerOnce(t, h, "peer-one"))
	if len(before) < 2 {
		t.Skipf("unscoped answer only reached %v; nothing to narrow", before)
	}
	if err := h.SetPatronScope(PatronScope{Interface: ifaces[0]}); err != nil {
		t.Fatal(err)
	}
	after := interfacesIn(t, answerOnce(t, h, "peer-two"))
	if len(after) != 1 || after[0] != ifaces[0] {
		t.Fatalf("after scoping to %q the answer still offers %v (it offered %v before)", ifaces[0], after, before)
	}
}

// §9.4 keeps client identifiers out of logs, and a candidate address is one.
// Scoping added a log line, so it gets the same check the rest of the package
// has: the interface NAME is operational fact, an address is not.
func TestScopingLogsTheInterfaceAndNeverAnAddress(t *testing.T) {
	ifaces := gatherableIfaces(t)
	if len(ifaces) == 0 {
		t.Skip("no gatherable interface on this host")
	}
	logs := &syncBuffer{}
	h := newHubLogging(t, logs)
	if err := h.SetPatronScope(PatronScope{Interface: ifaces[0]}); err != nil {
		t.Fatal(err)
	}
	answerOnce(t, h, "peer")
	out := logs.String()
	if !strings.Contains(out, ifaces[0]) {
		t.Fatalf("the scope change was not logged at all:\n%s", out)
	}
	for _, ip := range candidateAddrs(answerOnce(t, h, "peer-two")) {
		if strings.Contains(out, ip.String()) {
			t.Fatalf("a candidate address reached the log: %s", ip)
		}
	}
}
