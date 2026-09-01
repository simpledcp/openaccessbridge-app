package ipc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSharedSocketDirIsLeftAsProvisioned covers the appliance defect where each
// process "corrected" the shared runtime directory to its own default mode and
// locked the others out after a restart (§3.5): media tightening it to 0700
// left the root helper — which holds no CAP_DAC_OVERRIDE — unable to create its
// socket, and the helper widening it to 0750 left core unable to create the
// §9.4 debug-capture sink. A directory that already exists is the platform's
// (RuntimeDirectory=, a tmpfs volume) and must keep the mode it was given.
func TestSharedSocketDirIsLeftAsProvisioned(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	// umask-proof: the test is about what Listen does to the mode, not mkdir.
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		opts ListenOptions
	}{
		{"owner-only listener (media)", ListenOptions{}},
		{"group listener (firewall helper)", ListenOptions{Mode: 0o660, DirMode: 0o750}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := filepath.Join(dir, strings.Fields(tc.name)[0]+".sock")
			srv, err := ListenWithOptions(sock, func(Message) *Message { return nil }, tc.opts)
			if err != nil {
				t.Fatalf("ListenWithOptions: %v", err)
			}
			defer srv.Close()
			di, err := os.Stat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := di.Mode().Perm(); got != 0o770 {
				t.Errorf("shared socket dir mode changed to %o; a provisioned directory must keep its mode (0770)", got)
			}
		})
	}
}

// TestCreatedSocketDirGetsTheRequestedMode pins the other half: when Listen is
// the one creating the directory (a dev host, the OS temp dir), the requested
// mode is applied regardless of umask — the historical behaviour.
func TestCreatedSocketDirGetsTheRequestedMode(t *testing.T) {
	old := umask(0o077)
	defer umask(old)

	dir := filepath.Join(t.TempDir(), "fresh")
	sock := filepath.Join(dir, "helper.sock")
	srv, err := ListenWithOptions(sock, func(Message) *Message { return nil }, ListenOptions{
		Mode: 0o660, DirMode: 0o750,
	})
	if err != nil {
		t.Fatalf("ListenWithOptions: %v", err)
	}
	defer srv.Close()
	di, _ := os.Stat(dir)
	if got := di.Mode().Perm(); got != 0o750 {
		t.Errorf("created socket dir mode = %o, want 750 even under a 077 umask", got)
	}
}

// TestWorldWritableSocketDirIsRefused: a socket in a directory any local user
// can write to is a socket any local user can replace. That is refused with the
// reason, never silently used.
func TestWorldWritableSocketDirIsRefused(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(filepath.Join(dir, "x.sock"), func(Message) *Message { return nil })
	if err == nil {
		t.Fatal("Listen accepted a world-writable socket directory")
	}
	if !strings.Contains(err.Error(), "world-writable") {
		t.Errorf("refusal does not name the reason: %v", err)
	}
}

// TestSymlinkedSocketDirIsRefused: the socket path must not be redirectable
// through a link somebody else controls.
func TestSymlinkedSocketDirIsRefused(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := Listen(filepath.Join(link, "x.sock"), func(Message) *Message { return nil })
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("symlinked socket dir was not refused by name: %v", err)
	}
}

// TestSocketIsCreatedWithItsModeAndVerified: the socket comes out of bind(2)
// already at the requested mode (umask, not a chmod after the fact), and what
// sits at the path afterwards is verified to be this process's socket. A path
// that has been swapped for something else is refused by name.
func TestSocketIsCreatedWithItsModeAndVerified(t *testing.T) {
	old := umask(0o022)
	defer umask(old)

	dir := t.TempDir()
	sock := filepath.Join(dir, "helper.sock")
	srv, err := ListenWithOptions(sock, func(Message) *Message { return nil }, ListenOptions{Mode: 0o660})
	if err != nil {
		t.Fatalf("ListenWithOptions: %v", err)
	}
	defer srv.Close()
	si, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if si.Mode()&os.ModeSocket == 0 || si.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %v, want a socket with 0660 regardless of a 022 umask", si.Mode())
	}
	if got := umask(old); got != 0o022 {
		t.Errorf("umask left at %04o; Listen must restore it", got)
	}

	// The verification half, against a path that is not a socket at all.
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o660); err != nil {
		t.Fatal(err)
	}
	if err := verifySocketFile(plain, 0o660, 0); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("a regular file at the socket path was not refused by name: %v", err)
	}
	if err := verifySocketFile(sock, 0o600, 0); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("a socket with the wrong mode was not refused: %v", err)
	}
}

// TestCheckSharedSocketDirPolicy exercises the ownership policy against
// synthetic identities, since the real cases — a root-owned directory, one
// owned by the shared group — need accounts a test does not have.
func TestCheckSharedSocketDirPolicy(t *testing.T) {
	const self, other, sharedGID = 1901, 4242, 1901
	dirMode := os.ModeDir | 0o770
	cases := []struct {
		name string
		d    dirIdentity
		gid  int
		ok   bool
	}{
		{"owned by this process", dirIdentity{Mode: dirMode, UID: self, GID: self, OwnerKnown: true}, 0, true},
		{"owned by root (systemd, the container runtime)", dirIdentity{Mode: dirMode, UID: 0, GID: sharedGID, OwnerKnown: true}, 0, true},
		{"root-owned with the sticky bit (the Docker tmpfs volume)", dirIdentity{Mode: os.ModeDir | os.ModeSticky | 0o770, UID: 0, GID: sharedGID, OwnerKnown: true}, 0, true},
		{"root helper in a dir owned by the unprivileged account, shared group", dirIdentity{Mode: dirMode, UID: other, GID: sharedGID, OwnerKnown: true}, sharedGID, true},
		{"other owner, no shared group configured", dirIdentity{Mode: dirMode, UID: other, GID: sharedGID, OwnerKnown: true}, 0, false},
		{"other owner, other group", dirIdentity{Mode: dirMode, UID: other, GID: other, OwnerKnown: true}, sharedGID, false},
		{"world-writable, even when owned by us", dirIdentity{Mode: os.ModeDir | 0o777, UID: self, GID: self, OwnerKnown: true}, 0, false},
		{"world-writable with the sticky bit is still refused", dirIdentity{Mode: os.ModeDir | os.ModeSticky | 0o1777, UID: 0, GID: 0, OwnerKnown: true}, 0, false},
		{"symlink", dirIdentity{Mode: os.ModeSymlink | 0o777, UID: self, GID: self, OwnerKnown: true}, 0, false},
		{"a regular file where the dir should be", dirIdentity{Mode: 0o600, UID: self, GID: self, OwnerKnown: true}, 0, false},
		{"owner unknown (non-Unix): mode is still judged", dirIdentity{Mode: dirMode, OwnerKnown: false}, 0, true},
		{"owner unknown, world-writable", dirIdentity{Mode: os.ModeDir | 0o777, OwnerKnown: false}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkSharedSocketDir("/run/openaccessbridge", tc.d, self, tc.gid)
			if (err == nil) != tc.ok {
				t.Errorf("accepted=%v (err=%v), want accepted=%v", err == nil, err, tc.ok)
			}
		})
	}
}
