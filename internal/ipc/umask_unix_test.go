//go:build unix

package ipc

import "syscall"

// umask sets the process umask and returns the previous one, for the test that
// proves a created socket directory gets its requested mode regardless of it.
func umask(mask int) int { return syscall.Umask(mask) }
