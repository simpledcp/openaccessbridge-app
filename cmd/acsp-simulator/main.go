// Command acsp-simulator is a fake cinema-server / IMB that emits realistic
// ST 430-10 ACSP behavior so the caption path can be developed and
// regression-tested without a real server (§11.1).
//
// PROVISIONAL: the modeled wire behavior follows public ISDCF implementation
// notes (§5.1–5.2), clearly seamed so the real Phase 0A wire contract can be
// swapped in. The scenario engine lives in internal/sim; this binary runs the
// scenarios standalone and reports pass/fail (the same suite CI runs headless).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
	"github.com/simpledcp/captionbridge/internal/sim"
)

// logT implements sim.T for standalone runs, recording failures.
type logT struct {
	name   string
	failed bool
}

func (l *logT) Helper() {}
func (l *logT) Errorf(format string, args ...any) {
	l.failed = true
	fmt.Fprintf(os.Stderr, "  [%s] FAIL: "+format+"\n", append([]any{l.name}, args...)...)
}
func (l *logT) Fatalf(format string, args ...any) {
	l.Errorf(format, args...)
}

func main() {
	scenario := flag.String("scenario", "all", "scenario name to run, or 'all'")
	list := flag.Bool("list", false, "list available scenarios and exit")
	flag.Parse()

	scenarios := sim.All()
	if *list {
		fmt.Printf("acsp-simulator %s — %d scenarios:\n", buildinfo.Version, len(scenarios))
		for _, s := range scenarios {
			fmt.Printf("  %-32s %s\n", s.Name, s.Desc)
		}
		return
	}

	var ran, failed int
	for _, s := range scenarios {
		if *scenario != "all" && s.Name != *scenario {
			continue
		}
		ran++
		t := &logT{name: s.Name}
		h := sim.NewHarness(s.Opts)
		s.Run(t, h)
		status := "PASS"
		if t.failed {
			status = "FAIL"
			failed++
		}
		fmt.Printf("[%s] %s\n", status, s.Name)
	}
	if ran == 0 {
		fmt.Fprintf(os.Stderr, "no scenario matched %q\n", *scenario)
		os.Exit(2)
	}
	fmt.Printf("\n%d run, %d failed\n", ran, failed)
	if failed > 0 {
		os.Exit(1)
	}
}
