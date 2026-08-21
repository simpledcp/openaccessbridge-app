package acsp

import (
	"context"
	"testing"

	"github.com/simpledcp/openaccessbridge/internal/caption/resolver"
)

// TestTheAnnouncedServerFamilyReachesTheResolver closes the gap between a field
// that was on the wire and a field anything read.
//
// Request.ServerFamily existed in wire.go from the start and handleAnnounce took
// only ServerID, so a server that identified itself was ignored and §5.3's
// "classify by server family" had nothing to classify by. The plumbing is the
// whole feature: the resolver cannot ask who it is talking to.
func TestTheAnnouncedServerFamilyReachesTheResolver(t *testing.T) {
	res := resolver.NewPluggable(&resolver.MemoryFetcher{}, nil, nil)
	m := New(Options{Resolver: res})
	m.Handle(Request{Type: ReqAnnounce, ServerID: "srv-1", ServerFamily: "GDC"})

	if got := res.ServerFamily(); got != "GDC" {
		t.Fatalf("resolver family = %q, want %q — the announced family was dropped", got, "GDC")
	}
	if got := m.State().ServerFamily; got != "GDC" {
		t.Fatalf("state family = %q, want %q", got, "GDC")
	}

	// An unresolvable locator from this server is now attributable.
	if _, err := res.Resolve(context.Background(), "dcp://vendor/asset"); err == nil {
		t.Fatal("expected an error for a vendor-specific locator")
	}
	got := res.UnresolvedLocators()
	if len(got) != 1 || got[0].Family != "GDC" {
		t.Fatalf("observation not attributed to the announcing server: %+v", got)
	}
}

// TestTheConfiguredFamilyStandsInWhenTheServerDeclaresNone: ServerFamily is a
// PROVISIONAL wire field on an unconfirmed contract (B1), so most servers will
// declare nothing. The auditorium profile's value — an installer's answer to a
// setup question — is what attributes the observation then.
func TestTheConfiguredFamilyStandsInWhenTheServerDeclaresNone(t *testing.T) {
	res := resolver.NewPluggable(&resolver.MemoryFetcher{}, nil, nil)
	m := New(Options{Resolver: res, ServerFamily: "Dolby IMS"})
	m.Handle(Request{Type: ReqAnnounce, ServerID: "srv-1"}) // no family declared

	if got := res.ServerFamily(); got != "Dolby IMS" {
		t.Fatalf("resolver family = %q, want the configured %q", got, "Dolby IMS")
	}
}

// ...and the server WINS when it does declare one: the configured value is an
// operator's answer to a question, the announced value is the server speaking
// for itself. A venue that mis-answered the setup wizard must not have its
// mistake stamped onto evidence the server itself contradicted.
func TestTheAnnouncedFamilyOverridesTheConfiguredOne(t *testing.T) {
	res := resolver.NewPluggable(&resolver.MemoryFetcher{}, nil, nil)
	m := New(Options{Resolver: res, ServerFamily: "Dolby IMS"})
	m.Handle(Request{Type: ReqAnnounce, ServerID: "srv-1", ServerFamily: "GDC"})

	if got := res.ServerFamily(); got != "GDC" {
		t.Fatalf("resolver family = %q, want the announced %q", got, "GDC")
	}
}

// An appliance that knows neither records EMPTY, not "unknown". A placeholder
// compares equal across two appliances talking to two different vendors, which
// merges their evidence into one bucket and invents a shared problem.
func TestAnUnknownFamilyIsRecordedAsUnknown(t *testing.T) {
	res := resolver.NewPluggable(&resolver.MemoryFetcher{}, nil, nil)
	m := New(Options{Resolver: res})
	m.Handle(Request{Type: ReqAnnounce, ServerID: "srv-1"})

	if got := res.ServerFamily(); got != "" {
		t.Fatalf("family = %q, want empty — nothing declared one", got)
	}
	if got := m.State().ServerFamily; got != "" {
		t.Fatalf("state family = %q, want empty", got)
	}
}
