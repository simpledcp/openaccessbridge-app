//go:build !unix

package ipc

// umask is a no-op off Unix, where there is no process umask to restore.
func umask(int) int { return 0 }
