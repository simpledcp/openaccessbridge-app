//go:build !unix

package ipc

import (
	"net"
	"os"
)

// listenUnixWithMode has no umask to set off Unix; the mode is applied after
// bind, as it always was there (the appliance is Linux — DEVELOPMENT.md).
func listenUnixWithMode(path string, mode os.FileMode) (net.Listener, error) {
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// fileOwner has no uid/gid to report off Unix; ownership is then not judged.
func fileOwner(os.FileInfo) (uid, gid int, ok bool) { return 0, 0, false }
