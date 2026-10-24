package library

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
	"github.com/amwolff/yamp/internal/testutil"
)

const salt = "test-salt"

func newScanner(t *testing.T, lib string) (*Scanner, *store.Store) {
	t.Helper()
	ffmpeg, ffprobe := testutil.FFmpeg(t)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{
		Roots: []string{lib}, CacheDir: t.TempDir(), FFmpeg: ffmpeg,
		Prober: &probe.Prober{FFprobe: ffprobe}, Store: st,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Salt: func() string { return salt },
	})
	return s, st
}

func mediaByPath(t *testing.T, st *store.Store, lib string) map[string]db.Medium {
	t.Helper()
	rows, err := st.ListMediaForScan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]db.Medium{}
	for _, r := range rows {
		m, err := st.GetMedia(context.Background(), r.ID)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(lib, m.Path)
		out[filepath.ToSlash(rel)] = m
	}
	return out
}

func TestScanFixtures(t *testing.T) {
	lib := t.TempDir()
	testutil.MakeLibrary(t, lib, 2)
	s, st := newScanner(t, lib)
	ctx := context.Background()
	if err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}

	want := map[string]probe.Class{
		testutil.Direct:    probe.Direct,
		testutil.SlowStart: probe.Remux,
		testutil.MKV:       probe.Remux,
		testutil.AC3:       probe.AudioOnly,
		testutil.Hi10P:     probe.Transcode,
		testutil.HEVCHev1:  probe.Transcode,
		testutil.VP9:       probe.Conditional,
		testutil.HDR:       probe.Conditional,
	}
	media := mediaByPath(t, st, lib)
	if len(media) != len(want) {
		t.Fatalf("indexed %d files, want %d: %v", len(media), len(want), keys(media))
	}
	for path, class := range want {
		m, ok := media[path]
		if !ok {
			t.Errorf("%s not indexed", path)
			continue
		}
		if m.Playability != string(class) {
			t.Errorf("%s: class %s, want %s", path, m.Playability, class)
		}
		if m.Pid != privacy.PID(salt, m.Fingerprint) {
			t.Errorf("%s: pid does not match the fingerprint", path)
		}
		thumb, err := os.ReadFile(ThumbPath(s.cfg.CacheDir, m.ID))
		if err != nil || m.HasThumb != 1 || !bytes.HasPrefix(thumb, []byte{0xFF, 0xD8}) {
			t.Errorf("%s: no JPEG thumbnail (has_thumb=%d, err=%v)", path, m.HasThumb, err)
		}
	}
	if m := media[testutil.HDR]; m.Hdr != 1 {
		t.Error("HDR fixture not marked HDR")
	}
	if m := media[testutil.Direct]; m.Mime != "video/mp4" || !strings.HasPrefix(m.Codecs, "avc1.") {
		t.Errorf("direct fixture: mime %q codecs %q", m.Mime, m.Codecs)
	}
	if m := media[testutil.MKV]; m.RelDir != "Shows/Season 1" || m.Title != "remux S01E01" {
		t.Errorf("mkv fixture: rel_dir %q title %q", m.RelDir, m.Title)
	}

	subs, err := st.ListSubtitles(ctx, media[testutil.MKV].ID)
	if err != nil || len(subs) != 1 || subs[0].Lang != "eng" {
		t.Fatalf("subtitles = %+v, %v", subs, err)
	}
	if vtt, _ := os.ReadFile(subs[0].Path); !bytes.HasPrefix(vtt, []byte("WEBVTT")) || !bytes.Contains(vtt, []byte("Hello")) {
		t.Fatalf("bad WebVTT: %q", vtt)
	}

	status := s.Status()
	if status.Added != len(want) || status.Running {
		t.Errorf("status = %+v", status)
	}
	if !containsIssue(status.Issues, testutil.AudioOnly, "no video stream") {
		t.Errorf("audio-only file not reported: %q", status.Issues)
	}
}

func TestRescanRenameMissingAndDuplicate(t *testing.T) {
	lib := t.TempDir()
	testutil.MakeLibrary(t, lib, 1)
	s, st := newScanner(t, lib)
	ctx := context.Background()
	if err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	before := mediaByPath(t, st, lib)

	// Rename: same row, new path and title.
	if err := os.Rename(filepath.Join(lib, testutil.Direct), filepath.Join(lib, "Movies/renamed movie.mp4")); err != nil {
		t.Fatal(err)
	}
	// Delete: the row becomes missing.
	slow := filepath.Join(lib, testutil.SlowStart)
	slowData, err := os.ReadFile(slow) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(slow); err != nil {
		t.Fatal(err)
	}
	// Duplicate: a copy of an indexed file is reported, not indexed.
	if err := os.WriteFile(filepath.Join(lib, "Clips/copy.webm"), mustRead(t, filepath.Join(lib, testutil.VP9)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	after := mediaByPath(t, st, lib)
	renamed, ok := after["Movies/renamed movie.mp4"]
	if !ok || renamed.ID != before[testutil.Direct].ID || renamed.Title != "renamed movie" {
		t.Fatalf("rename not tracked: %+v", renamed)
	}
	if renamed.Pid != before[testutil.Direct].Pid {
		t.Fatal("pid changed after a rename")
	}
	if after[testutil.SlowStart].Status != "missing" {
		t.Fatalf("deleted file status = %q", after[testutil.SlowStart].Status)
	}
	if _, ok := after["Clips/copy.webm"]; ok {
		t.Fatal("duplicate indexed")
	}
	if !containsIssue(s.Status().Issues, "copy.webm", "same content") {
		t.Errorf("duplicate not reported: %q", s.Status().Issues)
	}

	// Restore: the row comes back.
	if err := os.WriteFile(slow, slowData, 0o600); err != nil { //nolint:gosec // G703: test fixture
		t.Fatal(err)
	}
	if err := s.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if got := mediaByPath(t, st, lib)[testutil.SlowStart]; got.Status != "ok" || got.ID != before[testutil.SlowStart].ID {
		t.Fatalf("restored file: %+v", got)
	}

	// A new salt changes every pid and keeps them unique.
	if err := RotateSalt(ctx, st, "another salt"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, m := range mediaByPath(t, st, lib) {
		if m.Pid != privacy.PID("another salt", m.Fingerprint) || seen[m.Pid] {
			t.Fatalf("bad pid after rotation: %+v", m.Pid)
		}
		seen[m.Pid] = true
	}
	if v, _ := st.GetSetting(ctx, "salt"); v != "another salt" {
		t.Fatalf("salt setting = %q", v)
	}
}

func TestTitle(t *testing.T) {
	for in, want := range map[string]string{
		"/a/The.Movie.2024.mkv":    "The Movie 2024",
		"/a/Some Film (1999).mp4":  "Some Film (1999)",
		"/a/show_s01e01.mkv":       "show s01e01",
		"/a/keep.dots in name.mp4": "keep.dots in name",
	} {
		if got := Title(in); got != want {
			t.Errorf("Title(%q) = %q, want %q", in, got, want)
		}
	}
}

func containsIssue(issues []string, parts ...string) bool {
	for _, is := range issues {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(is, p)
		}
		if ok {
			return true
		}
	}
	return false
}

func keys(m map[string]db.Medium) []string {
	var k []string
	for p := range m {
		k = append(k, p)
	}
	return k
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: test fixture
	if err != nil {
		t.Fatal(err)
	}
	return b
}
