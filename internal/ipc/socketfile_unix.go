//go:build unix

package ipc

import (
	"net"
	"os"
	"syscall"
)

// listenUnixWithMode binds a Unix socket that is CREATED with its final mode,
// rather than created wide and narrowed afterwards. bind(2) gives the socket
// inode 0777 minus the umask, so the umask is the only knob that sets the mode
// atomically; a chmod by path after bind follows symlinks, and in a directory
// another account can write to, the window between bind and chmod is one in
// which that account can swap the fresh socket for a link to a file it wants
// the privileged helper to re-mode.
//
// The umask is process-wide. Both helpers call Listen once, at startup, from
// main; a file created by another goroutine inside this microsecond window would
// merely come out narrower than asked.
func listenUnixWithMode(path string, mode os.FileMode) (net.Listener, error) {
	old := syscall.Umask(int(0o777 &^ mode.Perm()))
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}

// fileOwner reports the uid and gid that own a file, as the kernel records
// them. The third result is false when the platform's FileInfo carries no
// ownership, in which case ownership is not judged.
func fileOwner(info os.FileInfo) (uid, gid int, ok bool) {
	st, isStat := info.Sys().(*syscall.Stat_t)
	if !isStat {
		return 0, 0, false
	}
	return int(st.Uid), int(st.Gid), true
}
