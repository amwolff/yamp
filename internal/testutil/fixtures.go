// Package testutil generates small media fixtures for integration tests.
package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Fixture names, one for each playback class (see probe.Classify).
const (
	Direct    = "Movies/direct.mp4"
	SlowStart = "Movies/slow start.mp4"
	MKV       = "Shows/Season 1/remux.S01E01.mkv"
	AC3       = "Shows/Season 1/ac3.S01E02.mkv"
	Hi10P     = "Movies/hi10p.mkv"
	HEVCHev1  = "Movies/hevc_hev1.mp4"
	VP9       = "Clips/clip.webm"
	HDR       = "Movies/hdr.mp4"
	NotAVideo = "Movies/readme.txt"
	AudioOnly = "Clips/song.mp4"
)

// FFmpeg returns the paths of ffmpeg and ffprobe, or skips the test.
func FFmpeg(t testing.TB) (ffmpeg, ffprobe string) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found")
	}
	ffprobe, err = exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found")
	}
	return ffmpeg, ffprobe
}

// MakeLibrary writes the fixtures (seconds long) into dir.
func MakeLibrary(t testing.TB, dir string, seconds int) {
	t.Helper()
	ffmpeg, _ := FFmpeg(t)
	d := strconv.Itoa(seconds)
	v := func(size, rate string) []string {
		return []string{"-f", "lavfi", "-i", "testsrc2=size=" + size + ":rate=" + rate + ":duration=" + d}
	}
	a := func(freq string) []string {
		return []string{"-f", "lavfi", "-i", "sine=frequency=" + freq + ":sample_rate=48000:duration=" + d}
	}
	srt := filepath.Join(t.TempDir(), "subs.srt")
	if err := os.WriteFile(srt, []byte("1\n00:00:00,500 --> 00:00:01,500\nHello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	x264 := []string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p"}
	jobs := map[string][]string{
		Direct:    cat(v("320x180", "25"), a("440"), x264, []string{"-c:a", "aac", "-movflags", "+faststart"}),
		SlowStart: cat(v("320x180", "25"), a("550"), x264, []string{"-c:a", "aac"}),
		MKV: cat(v("320x180", "24"), a("660"), []string{"-i", srt, "-map", "0", "-map", "1", "-map", "2"}, x264,
			[]string{"-c:a", "aac", "-c:s", "srt", "-metadata:s:s:0", "language=eng"}),
		AC3: cat(v("320x180", "24"), a("330"), x264, []string{"-c:a", "ac3", "-ac", "6"}),
		Hi10P: cat(v("320x180", "24"), a("770"),
			[]string{"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "-c:a", "aac"}),
		HEVCHev1: cat(v("320x180", "24"), a("880"), []string{"-c:v", "libx265", "-preset", "ultrafast",
			"-pix_fmt", "yuv420p", "-tag:v", "hev1", "-x265-params", "log-level=error", "-c:a", "aac", "-movflags", "+faststart"}),
		VP9: cat(v("320x180", "30"), a("990"), []string{"-c:v", "libvpx-vp9", "-deadline", "realtime",
			"-cpu-used", "8", "-c:a", "libopus"}),
		HDR: cat(v("640x360", "24"), a("220"), []string{"-c:v", "libx265", "-preset", "ultrafast",
			"-pix_fmt", "yuv420p10le", "-tag:v", "hvc1",
			"-x265-params", "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
			"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
			"-c:a", "aac", "-movflags", "+faststart"}),
		AudioOnly: cat(a("440"), []string{"-c:a", "aac"}),
	}
	for name, args := range jobs {
		out := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(out), 0o750); err != nil {
			t.Fatal(err)
		}
		full := append(append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, args...), out)
		cmd := exec.Command(ffmpeg, full...) //nolint:gosec // G204: test fixtures
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("make %s: %v\n%s", name, err, b)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, NotAVideo), []byte("not a video"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Probe runs ffprobe with the given arguments on path.
func Probe(t testing.TB, path string, args ...string) string {
	t.Helper()
	_, ffprobe := FFmpeg(t)
	full := append(append([]string{"-v", "error"}, args...), path)
	b, err := exec.Command(ffprobe, full...).Output() //nolint:gosec // G204: test helper
	if err != nil {
		t.Fatalf("ffprobe %s: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func cat(parts ...[]string) []string {
	var out []string
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
