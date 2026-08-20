package debugcapture

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultProcMounts is the kernel's mount table.
const DefaultProcMounts = "/proc/mounts"

// memoryBackedFS are the filesystems that hold nothing across a reboot. §9.4
// requires the debug sink to be one of them ("a bounded tmpfs"), because
// "auto-delete on reboot" is then a property of the medium rather than a promise
// the software has to keep.
var memoryBackedFS = map[string]bool{
	"tmpfs":    true,
	"ramfs":    true,
	"devtmpfs": true,
}

// IsTmpfs reports whether dir sits on a memory-backed filesystem, by
// INTROSPECTING the mount table — the same self-introspection stance the security
// verification pass takes (§9.3). It resolves symlinks and matches the longest
// mount point that is a path prefix of dir, so a nested mount wins over its
// parent.
func IsTmpfs(dir, procMounts string) (bool, error) {
	if procMounts == "" {
		procMounts = DefaultProcMounts
	}
	target, err := resolveExisting(dir)
	if err != nil {
		return false, err
	}
	f, err := os.Open(procMounts)
	if err != nil {
		return false, fmt.Errorf("the mount table could not be read: %w", err)
	}
	defer f.Close()

	bestLen := -1
	bestFS := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 3 {
			continue
		}
		mount := unescapeMount(fields[1])
		if !underMount(target, mount) {
			continue
		}
		if len(mount) > bestLen {
			bestLen, bestFS = len(mount), fields[2]
		}
	}
	if err := sc.Err(); err != nil {
		return false, fmt.Errorf("the mount table could not be read: %w", err)
	}
	if bestLen < 0 {
		return false, fmt.Errorf("no mount point covers %q", target)
	}
	return memoryBackedFS[bestFS], nil
}

// resolveExisting resolves symlinks on the longest existing prefix of path, so a
// capture directory that has not been created yet is still attributed to the
// right mount.
func resolveExisting(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no path given")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	probe := filepath.Clean(abs)
	for {
		if resolved, err := filepath.EvalSymlinks(probe); err == nil {
			// Re-attach the part that does not exist yet; it cannot cross a mount
			// point that does not exist either.
			rest := strings.TrimPrefix(filepath.Clean(abs), probe)
			return filepath.Clean(resolved + rest), nil
		}
		parent := filepath.Dir(probe)
		if parent == probe {
			return filepath.Clean(abs), nil
		}
		probe = parent
	}
}

// underMount reports whether path is the mount point or lies beneath it.
func underMount(path, mount string) bool {
	if mount == "/" {
		return true
	}
	mount = strings.TrimSuffix(mount, "/")
	return path == mount || strings.HasPrefix(path, mount+string(filepath.Separator))
}

// unescapeMount decodes the octal escapes /proc/mounts uses for spaces, tabs,
// newlines, and backslashes in mount points.
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
