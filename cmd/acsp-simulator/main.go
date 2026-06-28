// Command acsp-simulator is a fake cinema-server / IMB that emits realistic
// ST 430-10 ACSP behavior so the caption path can be developed and
// regression-tested without a real server (§11.1).
//
// PROVISIONAL: the wire behavior modeled here is built from public ISDCF
// implementation notes (§5.1–5.2), not the licensed standard. It is clearly
// seamed so the real Phase 0A wire contract can be swapped in. The full scenario
// engine lives in internal/sim; this entrypoint exposes it as a runnable process.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/simpledcp/captionbridge/internal/buildinfo"
)

func main() {
	scenario := flag.String("scenario", "happy-path", "scenario name to run (see internal/sim)")
	list := flag.Bool("list", false, "list available scenarios and exit")
	flag.Parse()

	if *list {
		fmt.Println("acsp-simulator", buildinfo.Version)
		fmt.Println("scenarios are defined in internal/sim and exercised by the CI scenario suite.")
		return
	}

	// The standalone driver is wired to internal/sim in the simulator milestone.
	fmt.Fprintf(os.Stdout, "acsp-simulator %s — scenario %q (standalone runner pending internal/sim)\n",
		buildinfo.Version, *scenario)
}
