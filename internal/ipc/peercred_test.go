package ipc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGroupSocketModeAndOwnership covers the deployment defect where the root
// firewall helper created an owner-only socket that the unprivileged core could
// never connect to (§3.5). Chowning to a group needs privileges we do not have
// in tests, so assert the half that is testable unprivileged: the requested
// modes are actually applied to both the socket and its directory.
func TestGroupSocketModeAndOwnership(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	sock := filepath.Join(dir, "helper.sock")
	srv, err := ListenWithOptions(sock, func(Message) *Message { return nil }, ListenOptions{
		Mode: 0o660, DirMode: 0o750,
	})
	if err != nil {
		t.Fatalf("ListenWithOptions: %v", err)
	}
	defer srv.Close()

	si, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if got := si.Mode().Perm(); got != 0o660 {
		t.Errorf("socket mode = %o, want 660 (group must reach the privileged helper)", got)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o750 {
		t.Errorf("socket dir mode = %o, want 750 (group must traverse to the socket)", got)
	}
}

// TestListenDefaultsToOwnerOnly pins the historical posture: plain Listen stays
// 0700 dir / 0600 socket, so widening access is always an explicit choice.
func TestListenDefaultsToOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	sock := filepath.Join(dir, "helper.sock")
	srv, err := Listen(sock, func(Message) *Message { return nil })
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer srv.Close()

	si, _ := os.Stat(sock)
	if got := si.Mode().Perm(); got != 0o600 {
		t.Errorf("default socket mode = %o, want 600", got)
	}
	di, _ := os.Stat(dir)
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("default socket dir mode = %o, want 700", got)
	}
}

// TestAdmissionPolicy proves the privileged helper attributes its callers: a
// policy that refuses the peer drops the connection before any frame is handled,
// and permissions alone are not the boundary (§3.5).
func TestAdmissionPolicy(t *testing.T) {
	handled := false
	sock := filepath.Join(t.TempDir(), "helper.sock")
	srv, err := ListenWithOptions(sock, func(m Message) *Message {
		handled = true
		return &Message{Type: TypePong, ID: m.ID}
	}, ListenOptions{AllowPeer: func(Peer) bool { return false }})
	if err != nil {
		t.Fatalf("ListenWithOptions: %v", err)
	}
	defer srv.Close()

	cli, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Request(Message{Type: TypePing, ID: "1"}); err == nil {
		t.Fatal("refused peer got a response; the admission policy is not enforced")
	}
	if handled {
		t.Error("handler ran for a refused peer")
	}
	if srv.Rejected() != 1 {
		t.Errorf("Rejected() = %d, want 1", srv.Rejected())
	}
}

// TestAdmitsOwner is the positive case: the same-uid peer (core and helper both
// under one dev user, and every in-process test) is admitted.
func TestAdmitsOwner(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "helper.sock")
	srv, err := ListenWithOptions(sock, func(m Message) *Message {
		return &Message{Type: TypePong, ID: m.ID}
	}, ListenOptions{AllowPeer: AllowOwnerOrGroup(0)})
	if err != nil {
		t.Fatalf("ListenWithOptions: %v", err)
	}
	defer srv.Close()

	cli, err := Dial(sock)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer cli.Close()
	resp, err := cli.Request(Message{Type: TypePing, ID: "1"})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if resp.Type != TypePong {
		t.Fatalf("got %+v, want pong", resp)
	}
}

// TestAllowOwnerOrGroup checks the policy itself against synthetic peers, since
// a real cross-user connection needs two accounts.
func TestAllowOwnerOrGroup(t *testing.T) {
	self := os.Getuid()
	const sharedGID = 4242
	allow := AllowOwnerOrGroup(sharedGID)

	cases := []struct {
		name string
		peer Peer
		want bool
	}{
		{"same uid", Peer{UID: self, GID: 9999}, true},
		{"other uid in shared group", Peer{UID: self + 1, GID: sharedGID}, true},
		{"other uid, other group", Peer{UID: self + 1, GID: 9999}, false},
		{"root outside the shared group", Peer{UID: 0, GID: 0}, self == 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := allow(tc.peer); got != tc.want {
				t.Errorf("allow(%+v) = %v, want %v", tc.peer, got, tc.want)
			}
		})
	}

	// gid 0 must not become a wildcard that admits every root-group process.
	if AllowOwnerOrGroup(0)(Peer{UID: self + 1, GID: 0}) {
		t.Error("AllowOwnerOrGroup(0) admitted a foreign peer by group")
	}
}

// An over-long socket path fails bind(2) with EINVAL ("invalid argument"),
// which says nothing about the real cause. A long RuntimeDirectory on the
// appliance would hit this, so report the actual limit instead.
func TestSocketPathLengthIsReportedClearly(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("d", maxSocketPath), "x.sock")
	_, err := Listen(long, func(Message) *Message { return nil })
	if err == nil {
		t.Fatal("expected an error for an over-long socket path")
	}
	if !strings.Contains(err.Error(), "kernel limit") {
		t.Errorf("error does not explain the path-length limit: %v", err)
	}
	if _, err := Dial(long); err == nil || !strings.Contains(err.Error(), "kernel limit") {
		t.Errorf("Dial should reject the same path with the same explanation: %v", err)
	}

	// A path at the limit is accepted.
	dir := t.TempDir()
	name := strings.Repeat("s", maxSocketPath-len(dir)-len("/.sock")) + ".sock"
	ok := filepath.Join(dir, name)
	if len(ok) != maxSocketPath {
		t.Fatalf("test setup: built a %d-byte path, want %d", len(ok), maxSocketPath)
	}
	srv, err := Listen(ok, func(Message) *Message { return nil })
	if err != nil {
		t.Fatalf("a path exactly at the limit should bind: %v", err)
	}
	srv.Close()
}
