package main

// Core's half of the §9.4 support session: reading the configured endpoint out
// of the store, and turning the manager's state changes into an immediate
// posture reconciliation.

import (
	"flag"
	"strconv"
	"strings"

	"github.com/simpledcp/openaccessbridge/internal/config"
	"github.com/simpledcp/openaccessbridge/internal/logging"
	"github.com/simpledcp/openaccessbridge/internal/security/firewall"
	"github.com/simpledcp/openaccessbridge/internal/security/support"
)

// supportEndpoint reads the configured support destination. It is read on every
// call rather than cached, so an operator correcting it in the setup wizard does
// not need a restart — and so an endpoint REMOVED while a session is open takes
// the window's rule with it at the next reconciliation.
func supportEndpoint(store *config.SQLiteStore) support.Endpoint {
	host, _, _ := store.GetSetting(config.SettingSupportEndpointHost)
	iface, _, _ := store.GetSetting(config.SettingSupportEgressIface)
	portStr, _, _ := store.GetSetting(config.SettingSupportEndpointPort)
	port, err := strconv.Atoi(strings.TrimSpace(portStr))
	if err != nil {
		port = 0
	}
	return support.Endpoint{
		Iface: strings.TrimSpace(iface),
		Host:  strings.TrimSpace(host),
		Port:  port,
	}
}

// signalSupportChange asks the supervisor loop to reconcile NOW.
//
// The support window is a firewall rule with a deadline on it. Waiting for the
// next scheduled posture pass would leave an expired window permitting egress
// for up to one interval — a small number of seconds, but the wrong number,
// because the deadline is the promise the feature makes.
//
// It never blocks: the channel is buffered by one and a signal that finds it
// full is dropped, since a pending reconciliation will already pick up whatever
// state the manager is now in.
func (s *supervisor) signalSupportChange() {
	select {
	case s.supportChanged <- struct{}{}:
	default:
	}
}

// supportEgress is the firewall window a live session justifies, or nil.
//
// It asks the MANAGER rather than reading a stored flag: the manager re-checks
// the absolute deadline, so a window whose time has passed contributes no rule
// even when this pass runs before the expiry timer does. A reconciliation must
// never be the thing that keeps an expired window alive.
func (s *supervisor) supportEgress() *firewall.SupportEgress {
	if s.support == nil {
		return nil
	}
	return s.support.Egress()
}

// clearLegacySupportFlag removes a stored "support_mode = true" left by a build
// in which that key WAS the whole feature.
//
// It grants nothing now: there is no window, no deadline and no firewall rule
// behind it. Leaving it set would tell a future reader — and any tool that greps
// the config — that support mode is on, which is the kind of stale assertion
// this appliance spends its effort not making. Cleared loudly, into the same
// audit stream the real sessions use, so the change is visible to a review.
func clearLegacySupportFlag(store *config.SQLiteStore, log *logging.Logger) {
	v, ok, err := store.GetSetting(config.SettingSupportMode)
	if err != nil || !ok || v != "true" {
		return
	}
	_ = store.PutSetting(config.SettingSupportMode, "false")
	log.Warn(logging.EvSupportSessionClosed,
		logging.Field("phase", "legacy_flag_cleared"),
		logging.Field("detail", "a stored support_mode flag granted no window and has been cleared"))
}

// flagSet reports whether a flag was given on the command line, as opposed to
// carrying its default.
//
// It exists for exactly one decision: --auditorium's default is a plausible room
// name ("Auditorium 1"), and an appliance that was never told its room must not
// present that default to patrons as though it were the answer. Everywhere else
// a default is fine; here the default is indistinguishable from a real value and
// §8.5's whole purpose is telling those apart.
func flagSet(name string) bool {
	found := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}
