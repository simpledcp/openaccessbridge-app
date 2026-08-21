package verify

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/simpledcp/captionbridge/internal/model"
)

func sshCfg(t *testing.T, profile model.FirewallProfile, reader SSHReader) Config {
	t.Helper()
	return Config{
		Paths:          fixture(t, "0", "0", false),
		Profile:        profile,
		FirewallActive: true,
		SSH:            reader,
	}
}

// THE DEFECT THIS FILE EXISTS FOR. The §9.3 row was derived from a configuration
// setting nothing in the tree ever wrote, so it read "SSH disabled" on an
// appliance where sshd was genuinely running: fail-WRONG on a security row.
func TestSSHListeningOnANetworkIsReportedAsEnabled(t *testing.T) {
	rep := Run(sshCfg(t, model.ProfilePatronFacing,
		StaticSSHReader(SSHStatus{Listening: true, NetworkReachable: true, Port: 22}, nil)))

	if rep.SSHCorrect {
		t.Fatal("sshd is listening on a network and the appliance reports SSH correct")
	}
	if !rep.SSHIntrospected {
		t.Fatal("a measured state is not marked as measured")
	}
	if !hasWarning(rep, model.WarnSSHEnabled) {
		t.Fatalf("no warning for reachable SSH: %v", codesOf(rep))
	}
}

// Measured and nothing listening is a PASS — the row means something now.
func TestSSHNotListeningIsAPass(t *testing.T) {
	rep := Run(sshCfg(t, model.ProfilePatronFacing, StaticSSHReader(SSHStatus{Port: 22}, nil)))
	if !rep.SSHCorrect || !rep.SSHIntrospected {
		t.Fatalf("measured-and-closed reports correct=%v introspected=%v", rep.SSHCorrect, rep.SSHIntrospected)
	}
	if hasWarning(rep, model.WarnSSHEnabled) || hasWarning(rep, model.WarnSSHUnverified) {
		t.Fatalf("a closed, measured SSH raised %v", codesOf(rep))
	}
}

// D17 applied to SSH: an unmeasured way in is not a closed one — and asserting
// it is OPEN would be the mirror image of the defect this replaced. It reads
// unverified, and it does not pass.
func TestAnUnmeasurableSSHStateIsUnverifiedRatherThanEitherAnswer(t *testing.T) {
	rep := Run(sshCfg(t, model.ProfilePatronFacing,
		StaticSSHReader(SSHStatus{}, errors.New("/proc/net/tcp: permission denied"))))

	if rep.SSHCorrect {
		t.Fatal("an unmeasured SSH state passed the §9.3 row")
	}
	if rep.SSHIntrospected {
		t.Fatal("a failed measurement is marked as measured")
	}
	if !hasWarning(rep, model.WarnSSHUnverified) {
		t.Fatalf("no unverified warning: %v", codesOf(rep))
	}
	if hasWarning(rep, model.WarnSSHEnabled) {
		t.Fatal("an unmeasured state was reported as SSH being enabled — the mirror of the original defect")
	}
}

// The Lab profile PERMITS SSH (§9.2 "local SSH ok"), which is exactly where an
// unmeasured state is most dangerous: the profile rule would return "correct"
// for any SSH state at all, so without the introspection guard an appliance that
// measured nothing would report a verified pass. Found by a mutation that the
// production-profile test could not catch, because there the profile rule
// already fails.
func TestAnUnmeasurableStateIsNotCorrectEvenWhereTheProfileAllowsSSH(t *testing.T) {
	rep := Run(sshCfg(t, model.ProfileLab,
		StaticSSHReader(SSHStatus{}, errors.New("/proc/net/tcp: permission denied"))))

	if rep.SSHCorrect {
		t.Fatal("a profile that permits SSH reported the row correct without measuring it")
	}
	if !hasWarning(rep, model.WarnSSHUnverified) {
		t.Fatalf("no unverified warning on a permissive profile: %v", codesOf(rep))
	}
}

// And the same profile with a real measurement passes, so the guard above is not
// simply refusing everything.
func TestAPermissiveProfileStillPassesOnAMeasurement(t *testing.T) {
	rep := Run(sshCfg(t, model.ProfileLab,
		StaticSSHReader(SSHStatus{Listening: true, NetworkReachable: true, Port: 22}, nil)))
	if !rep.SSHCorrect {
		t.Fatal("Lab permits local SSH and a measured, listening sshd failed the row")
	}
}

// §9.2 permits local SSH in the Lab profile, and an appliance reachable only
// from its own console is not the re-entry path §9.4 is about.
func TestLoopbackOnlySSHIsNotAWayIn(t *testing.T) {
	rep := Run(sshCfg(t, model.ProfilePatronFacing,
		StaticSSHReader(SSHStatus{Listening: true, NetworkReachable: false, Port: 22}, nil)))
	if !rep.SSHCorrect {
		t.Fatal("SSH listening only on loopback was treated as a network way in")
	}
	if !rep.SSHListening {
		t.Fatal("the measurement lost the fact that something is listening")
	}
}

// The production reader must work unprivileged, because that is the whole reason
// this row can be measured at all. Run against the real host.
func TestTheProcReaderMeasuresThisHost(t *testing.T) {
	status, err := ProcSSHReader(DefaultSSHPort)()
	if err != nil {
		t.Skipf("this host's /proc/net/tcp could not be read: %v", err)
	}
	if status.Port != DefaultSSHPort {
		t.Fatalf("measured port %d", status.Port)
	}
	// Cross-check against the raw table rather than trusting the reader: count
	// LISTEN rows on port 22 independently.
	want := rawListeners(t, DefaultSSHPort)
	if status.Listening != want {
		t.Fatalf("reader says listening=%v, /proc says %v", status.Listening, want)
	}
	t.Logf("measured on this host: listening=%v network_reachable=%v", status.Listening, status.NetworkReachable)
}

// A port nothing uses must measure as closed, which proves the reader is reading
// rather than always answering the same way.
func TestAnUnusedPortMeasuresAsClosed(t *testing.T) {
	status, err := ProcSSHReader(64999)()
	if err != nil {
		t.Skipf("/proc/net/tcp: %v", err)
	}
	if status.Listening {
		t.Fatal("port 64999 reports a listener")
	}
}

// rawListeners counts LISTEN rows for a port straight out of /proc, so the test
// does not check the reader against itself.
func rawListeners(t *testing.T, port int) bool {
	t.Helper()
	for _, path := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(raw), "\n")[1:] {
			f := strings.Fields(line)
			if len(f) < 4 || f[3] != "0A" {
				continue
			}
			if _, p, ok := strings.Cut(f[1], ":"); ok && strings.EqualFold(p, "0016") && port == 22 {
				return true
			}
		}
	}
	return false
}

func hasWarning(rep Report, code string) bool {
	for _, w := range rep.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func codesOf(rep Report) []string {
	var out []string
	for _, w := range rep.Warnings {
		out = append(out, w.Code)
	}
	return out
}
