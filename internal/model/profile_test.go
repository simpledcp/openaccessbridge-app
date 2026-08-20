package model

import "testing"

// §8.4 makes admin/patron separation a hard boundary. A profile name that is not
// one of the four shipping postures — a typo, a hand edit, a future value from a
// newer build — must therefore be treated as PRODUCTION, the strictest posture.
// Returning false made web.validateBindSeparation skip the separation check
// entirely, so a corrupted profile string silently bought admin-on-the-patron-
// network: the exact configuration §8.4 exists to make impossible.
func TestUnknownProfileIsTreatedAsProduction(t *testing.T) {
	for _, p := range []FirewallProfile{
		"", "lab_but_typoed", "LAB", "patron facing", "low-cost-replacement", "future_profile",
	} {
		if p.Known() {
			t.Errorf("%q reported as a known profile", p)
		}
		if !p.Production() {
			t.Errorf("unknown profile %q is not treated as production — the §8.4 separation "+
				"check would be skipped for it", p)
		}
		if p.AdminAllowedOnPatronNet() {
			t.Errorf("unknown profile %q would permit admin on the patron network", p)
		}
	}
}

// The four shipping profiles keep their documented postures (§9.2, §12).
func TestKnownProfilePostures(t *testing.T) {
	cases := []struct {
		p          FirewallProfile
		production bool
		adminOnNet bool
	}{
		{ProfileLab, false, true},
		{ProfileBoothDiagnostic, false, true},
		{ProfilePatronFacing, true, false},
		{ProfileLowCostReplacement, true, false},
	}
	for _, c := range cases {
		if !c.p.Known() {
			t.Errorf("%q should be a known profile", c.p)
		}
		if got := c.p.Production(); got != c.production {
			t.Errorf("%q.Production() = %v, want %v", c.p, got, c.production)
		}
		if got := c.p.AdminAllowedOnPatronNet(); got != c.adminOnNet {
			t.Errorf("%q.AdminAllowedOnPatronNet() = %v, want %v", c.p, got, c.adminOnNet)
		}
	}
}
