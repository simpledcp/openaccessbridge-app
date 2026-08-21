package main

import (
	"context"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/config"
	"github.com/simpledcp/captionbridge/internal/logging"
	"github.com/simpledcp/captionbridge/internal/model"
	"github.com/simpledcp/captionbridge/internal/security/firewall"
	"github.com/simpledcp/captionbridge/internal/security/support"
)

func supportStore(t *testing.T) (*config.SQLiteStore, string) {
	t.Helper()
	dir := t.TempDir()
	store := testStore(t, model.AuditoriumProfile{AuditoriumID: "screen-3", CinemaServerIP: "10.10.20.15"})
	mustSetting(t, store, config.SettingTheaterInterface, "eth0")
	mustSetting(t, store, config.SettingClientInterface, "wlan0")
	mustSetting(t, store, config.SettingSupportEndpointHost, "203.0.113.10")
	mustSetting(t, store, config.SettingSupportEndpointPort, "443")
	mustSetting(t, store, config.SettingSupportEgressIface, "eth0")
	return store, dir
}

func derivedScript(t *testing.T, sup *supervisor) string {
	t.Helper()
	aud, _ := auditoriumProfile(sup.store)
	cfg := sup.derivedFirewallConfig("eth0", "wlan0", aud, false)
	script, err := firewall.Generate(cfg)
	if err != nil {
		t.Fatalf("generating the derived ruleset: %v", err)
	}
	return script
}

// The window has to reach the RULESET. A support session that opens a state
// variable and no rule is a feature that appears to work and permits nothing;
// one whose rule is never removed is a permanent egress hole opened by a support
// switch. Both directions are checked here.
func TestTheSupportWindowAppearsInAndDisappearsFromTheRuleset(t *testing.T) {
	store, dir := supportStore(t)
	sup := newSupervisor(t, store, dir)

	if script := derivedScript(t, sup); strings.Contains(script, "203.0.113.10") {
		t.Fatal("the support endpoint is in the ruleset with no session open")
	}

	if _, err := sup.support.Open(support.OpenRequest{Reason: "ticket 11", AdminUser: "tech"}); err != nil {
		t.Fatal(err)
	}
	script := derivedScript(t, sup)
	if !strings.Contains(script, "daddr 203.0.113.10 tcp dport 443 accept") {
		t.Fatalf("an open session did not put its window in the ruleset:\n%s", script)
	}

	if err := sup.support.Close(); err != nil {
		t.Fatal(err)
	}
	if script := derivedScript(t, sup); strings.Contains(script, "203.0.113.10") {
		t.Fatalf("closing the session left its egress rule behind:\n%s", script)
	}
}

// Opening a session must prompt an immediate reconciliation rather than waiting
// for the next scheduled posture pass. The deadline is the promise the feature
// makes, and a rule that lingers past it breaks that promise.
func TestOpeningASessionSignalsTheSupervisorToReconcile(t *testing.T) {
	store, dir := supportStore(t)
	sup := newSupervisor(t, store, dir)

	if _, err := sup.support.Open(support.OpenRequest{Reason: "ticket 12", AdminUser: "tech"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sup.supportChanged:
	default:
		t.Fatal("opening a session did not signal the supervisor; the rule would wait for the next posture pass")
	}
}

// A stored support_mode flag from an older build grants no window. Leaving it set
// would tell a future reader that support mode is on, so startup clears it.
func TestALegacySupportFlagIsClearedAndGrantsNothing(t *testing.T) {
	store, dir := supportStore(t)
	mustSetting(t, store, config.SettingSupportMode, "true")
	sup := newSupervisor(t, store, dir)
	clearLegacySupportFlag(store, logging.Nop())
	if v, _, _ := store.GetSetting(config.SettingSupportMode); v == "true" {
		t.Fatal("the legacy flag survived startup, so a future reader is told support mode is on")
	}
	sup.reconcile(context.Background())

	if st := sup.status(); st.SupportSession != model.TriNo {
		t.Fatalf("a legacy flag put the dashboard into support mode: %q", st.SupportSession)
	}
	if script := derivedScript(t, sup); strings.Contains(script, "203.0.113.10") {
		t.Fatal("a legacy flag opened an egress window with no session behind it")
	}
}
