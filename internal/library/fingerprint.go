package library

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os"
)

const chunk = 1 << 20

// Fingerprint is SHA-256 of the size and the first, middle and last MiB: cheap
// for large files and stable across renames.
func Fingerprint(path string) ([]byte, int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file found under a library root
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return nil, 0, err
	}
	size := st.Size()
	h := sha256.New()
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(size)) //nolint:gosec // G115: a file size is never negative
	h.Write(sz[:])
	if size <= 3*chunk {
		if _, err := io.Copy(h, f); err != nil {
			return nil, 0, err
		}
		return h.Sum(nil), size, nil
	}
	for _, off := range []int64{0, size/2 - chunk/2, size - chunk} {
		if _, err := io.Copy(h, io.NewSectionReader(f, off, chunk)); err != nil {
			return nil, 0, err
		}
	}
	return h.Sum(nil), size, nil
}
