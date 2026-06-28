// Package buildinfo carries version metadata shared by the binaries.
package buildinfo

// Version is the appliance software version. Overridable at build time via
// -ldflags "-X .../buildinfo.Version=...".
var Version = "0.1.0-dev"

// Components.
const (
	ComponentCore     = "core"
	ComponentMedia    = "media"
	ComponentFirewall = "firewall"
	ComponentSim      = "acsp-sim"
)
