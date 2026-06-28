package sim

import "testing"

// TestScenarioSuite runs the full ST 430-10 quirk scenario suite headless — the
// caption-path regression backbone wired into CI (§11.1). Every caption-path
// change must keep this green.
func TestScenarioSuite(t *testing.T) {
	scenarios := All()
	if len(scenarios) == 0 {
		t.Fatal("no scenarios registered")
	}
	seen := map[string]bool{}
	for _, sc := range scenarios {
		if seen[sc.Name] {
			t.Fatalf("duplicate scenario name %q", sc.Name)
		}
		seen[sc.Name] = true
		sc := sc
		t.Run(sc.Name, func(t *testing.T) {
			h := NewHarness(sc.Opts)
			sc.Run(t, h)
		})
	}
}

// TestScenarioCount guards that the documented quirk set stays covered: if a
// scenario is removed this fails, forcing a conscious decision (§11.1 lists the
// behaviors the simulator must reproduce).
func TestScenarioCount(t *testing.T) {
	const minScenarios = 17
	if n := len(All()); n < minScenarios {
		t.Fatalf("scenario suite shrank to %d (< %d); the quirk coverage is the regression backbone", n, minScenarios)
	}
}
