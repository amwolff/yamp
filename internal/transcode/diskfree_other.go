//go:build !linux && !darwin && !freebsd

package transcode

// FreeBytes is unsupported here, so the disk space check is skipped.
func FreeBytes(string) (int64, bool) { return 0, false }
