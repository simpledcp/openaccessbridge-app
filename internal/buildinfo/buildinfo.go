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
//
// It is set at build time by the Makefile and by CI
// (-ldflags "-X .../buildinfo.Build=..."). The zero default means "not set",
// and an appliance that does not know its own build refuses to judge an update
// rather than guessing which direction to fail in — see BuildNumber.
var Build = "0"

// BuildNumber parses Build, and reports whether this binary KNOWS its own build
// number.
//
// The second result is the load-bearing one. An appliance that does not know its
// build cannot tell an upgrade from a replay, and the update path must say so
// rather than pick a direction: treating an unknown build as 0 makes every
// bundle look newer (so a downgrade installs silently), and treating it as
// enormous makes every bundle look older (so nothing installs). Both are a
// verdict derived from an absence, which is the thing this appliance does not
// do — so Verify refuses instead, with that reason (§9.4, D32).
func BuildNumber() (int64, bool) {
	raw := strings.TrimSpace(Build)
	if raw == "" || raw == "0" {
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// Components.
const (
	ComponentCore     = "core"
	ComponentMedia    = "media"
	ComponentFirewall = "firewall"
	ComponentSim      = "acsp-sim"
)
