package webrtcpeer

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/model"
)

// §9.1 opens `iif <client> udp dport 10000-20000 accept` for patron media. Until
// this was set, pion bound to whatever ephemeral ports the kernel handed out, so
// on a real appliance with that ruleset loaded the phone's inbound connectivity
// checks arrived on ports the input chain drops — the firewall protecting a
// range nothing used.
//
// It cannot be caught on this host by running the appliance: without root no
// ruleset is ever loaded (B5), so the mismatch is invisible until it is in a
// booth.
func TestPatronMediaLandsInsideTheRangeTheFirewallOpens(t *testing.T) {
	h := newHub(t)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
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
	answer, err := h.Answer(context.Background(), "peer", model.StreamHI, offer.SDP, 0)
	if err != nil {
		t.Fatal(err)
	}

	ports := candidatePorts(t, answer)
	if len(ports) == 0 {
		t.Fatal("the answer carries no host candidate to check")
	}
	for _, p := range ports {
		if p < int(DefaultPortMin) || p > int(DefaultPortMax) {
			t.Fatalf("patron media on UDP %d, outside the %d-%d the firewall opens: "+
				"the phone's connectivity checks would be dropped",
				p, DefaultPortMin, DefaultPortMax)
		}
	}
}

// An explicit range is honoured, so a venue that has to move the range can move
// both halves together.
func TestAnExplicitMediaRangeIsHonoured(t *testing.T) {
	requireRoutableInterface(t)
	h, err := New(Options{PortMin: 15000, PortMax: 15100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	if err := h.Publish(model.StreamHI, newFakeEncoder()); err != nil {
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
	answer, err := h.Answer(context.Background(), "peer", model.StreamHI, offer.SDP, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range candidatePorts(t, answer) {
		if p < 15000 || p > 15100 {
			t.Fatalf("media on UDP %d, outside the configured 15000-15100", p)
		}
	}
}

// An inverted range is refused at construction rather than producing a peer
// whose media nothing can reach.
func TestAnInvertedMediaRangeIsRefused(t *testing.T) {
	if _, err := New(Options{PortMin: 20000, PortMax: 10000}); err == nil {
		t.Fatal("an inverted media port range was accepted")
	}
}

func candidatePorts(t *testing.T, sdp string) []int {
	t.Helper()
	var out []int
	for _, line := range strings.Split(sdp, "\n") {
		idx := strings.Index(line, "candidate:")
		if idx < 0 {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(line[idx:]))
		if len(fields) < 6 {
			continue
		}
		p, err := strconv.Atoi(fields[5])
		if err != nil {
			continue
		}
		out = append(out, p)
	}
	return out
}
