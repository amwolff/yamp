package library

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestFingerprint(t *testing.T) {
	dir := t.TempDir()
	big := bytes.Repeat([]byte("0123456789abcdef"), 5*chunk/16)
	a := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(a, big, 0o600); err != nil {
		t.Fatal(err)
	}
	fa, size, err := Fingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(big)) {
		t.Fatalf("size = %d", size)
	}

	// A rename keeps the fingerprint.
	b := filepath.Join(dir, "renamed.mkv")
	if err = os.Rename(a, b); err != nil {
		t.Fatal(err)
	}
	fb, _, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fa, fb) {
		t.Fatal("fingerprint changed after rename")
	}

	// A change in a sampled region changes it.
	big[len(big)-1] ^= 0xff
	if err = os.WriteFile(b, big, 0o600); err != nil {
		t.Fatal(err)
	}
	fc, _, err := Fingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(fa, fc) {
		t.Fatal("fingerprint did not change")
	}

	// Small files are hashed completely.
	s := filepath.Join(dir, "small.mp4")
	if err = os.WriteFile(s, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Fingerprint(s); err != nil {
		t.Fatal(err)
	}
}
