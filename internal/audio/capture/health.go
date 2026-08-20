package capture

// Health is a Source's own report of its device state (§6.3 "Presence / device
// health: input present, device connected, not failed" and §6.1 "Hot-plug aware:
// detect device connect/disconnect and surface it immediately"). It contains no
// audio and no measurements — only the device-level facts the source knows.
type Health struct {
	// Present reports that an input is configured and attached.
	Present bool
	// Connected reports that the device is currently usable (a USB dongle that
	// has been bumped out of its port is Present=false, Connected=false).
	Connected bool
	// Detail is a short, content-free explanation for a false value, shown to
	// staff verbatim (e.g. "device node disappeared").
	Detail string
}

// HealthReporter is an OPTIONAL capability a Source may implement. It is a
// separate interface, not a method on Source, so a source that cannot observe
// its device (or a third-party one) still satisfies Source.
//
// The pipeline consults it every analysis block: a source that implements it is
// authoritative about presence/connection, and one that does not falls back to
// "frames are arriving, so the device is delivering audio". The real ALSA source
// (hardware-gated, BLOCKERS B3/B4) implements this by watching its device node.
type HealthReporter interface {
	Health() Health
}

// Health reports the synthetic source's own state: present and connected while
// it is running, disconnected before Start and once Close has stopped it. It
// describes only the SYNTHETIC generator and makes no claim about any hardware.
func (s *SyntheticTone) Health() Health {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	switch {
	case !s.started:
		return Health{Detail: "synthetic source not started"}
	case s.closed:
		return Health{Detail: "synthetic source stopped"}
	}
	return Health{Present: true, Connected: true}
}
