// Package buildinfo carries version metadata shared by the binaries.
package buildinfo

import (
	"strconv"
	"strings"
)

// Version is the appliance software version. Overridable at build time via
// -ldflags "-X .../buildinfo.Version=...".
var Version = "0.1.0-dev"

// Build is the MONOTONIC build number the §9.4 update path orders upgrades by.
// Version strings are for people; comparing them is a source of bugs (is "1.10"
// newer than "1.9"?), so ordering is an integer the release process owns.
// Overridable at build time via -ldflags "-X .../buildinfo.Build=1420".
var Build = "0"

// BuildNumber is Build parsed, or 0 when it was never set or cannot be parsed.
//
// Zero is the SAFE value here and the choice is deliberate: with a build number
// of 0 every signed bundle is newer, so a development binary can verify real
// bundles. The alternative — treating an unparseable build as very large —
// would make an appliance refuse every legitimate update because of a build-flag
// mistake, and an operator who cannot verify a bundle is tempted to install it
// unverified.
func BuildNumber() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(Build), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Components.
const (
	ComponentCore     = "core"
	ComponentMedia    = "media"
	ComponentFirewall = "firewall"
	ComponentSim      = "acsp-sim"
)
