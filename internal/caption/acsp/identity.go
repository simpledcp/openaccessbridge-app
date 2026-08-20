package acsp

// IdentityState is the outcome of the §8.5 wrong-room / pinned-server-identity
// check. It deliberately distinguishes "verified" from "could not verify":
// §10.5 warns that the pinned identity may be only a weak composite (configured
// IP + server family + screen name) and "must be labelled as such in the UI so
// it is not mistaken for a security property the protocol does not provide".
// Collapsing these to a boolean is how a guard that never ran renders green.
type IdentityState string

const (
	// IdentityUnknown: no server has announced yet.
	IdentityUnknown IdentityState = "unknown"
	// IdentityUnpinned: this auditorium pins nothing, so there is nothing to
	// check. Honest, and a setup gap rather than a fault.
	IdentityUnpinned IdentityState = "unpinned"
	// IdentityUnverified: something was pinned, but the server announced nothing
	// to check it against. The guard did not run.
	IdentityUnverified IdentityState = "unverified"
	// IdentityMatch: every pin the server answered matched.
	IdentityMatch IdentityState = "match"
	// IdentityMismatch: at least one pin disagreed — the loud wrong-room case.
	IdentityMismatch IdentityState = "mismatch"
)

// Verified reports whether the wrong-room guard actually ran and passed. An
// unpinned or unanswerable identity is NOT verified.
func (s IdentityState) Verified() bool { return s == IdentityMatch }

// Identity returns the current §8.5 identity state.
func (m *Machine) Identity() IdentityState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identity
}
