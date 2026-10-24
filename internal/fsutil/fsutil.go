// Package fsutil has small file helpers for the cache directory.
package fsutil

import (
	"os"
	"path/filepath"
)

// Commit renames tmp to final, fsyncing it first if asked. fsync is off by
// default: the cache can be regenerated, and on ZFS fsyncing large files is slow.
func Commit(tmp, final string, fsync bool) error {
	if fsync {
		f, err := os.Open(tmp) //nolint:gosec // G304: a file that YAMP wrote in its cache directory
		if err != nil {
			return err
		}
		err = f.Sync()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// RemoveIfExists removes path and ignores a missing file.
func RemoveIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
