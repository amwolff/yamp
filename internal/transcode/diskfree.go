//go:build linux || darwin || freebsd

package transcode

import (
	"math"
	"syscall"
)

// FreeBytes returns the space available to unprivileged users on the file
// system of path. On ZFS this respects dataset quotas.
func FreeBytes(path string) (int64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	// The field types differ between operating systems.
	b := uint64(st.Bavail) * uint64(st.Bsize) //nolint:unconvert,gosec // G115: block counts and sizes are not negative
	return int64(min(b, math.MaxInt64)), true //nolint:gosec // G115: clamped
}
