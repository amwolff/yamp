package transcode_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amwolff/yamp/internal/library"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
	"github.com/amwolff/yamp/internal/testutil"
	"github.com/amwolff/yamp/internal/transcode"
)

type env struct {
	lib, cache string
	st         *store.Store
	queue      *transcode.Queue
	scanner    *library.Scanner
	prober     *probe.Prober
}

func setup(t *testing.T, seconds int) *env {
	t.Helper()
	ffmpeg, ffprobe := testutil.FFmpeg(t)
	ctx := context.Background()
	e := &env{lib: t.TempDir(), cache: t.TempDir(), prober: &probe.Prober{FFprobe: ffprobe}}
	testutil.MakeLibrary(t, e.lib, seconds)
	var err error
	e.st, err = store.Open(ctx, filepath.Join(t.TempDir(), "t.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.st.Close() })
	if err := transcode.SeedPresets(ctx, e.st); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	caps := transcode.Detect(ctx, transcode.ExecRunner, ffmpeg, "/dev/dri/renderD128")
	e.queue = transcode.NewQueue(transcode.QueueConfig{
		Store: e.st, Caps: caps, Prober: e.prober, FFmpeg: ffmpeg, CacheDir: e.cache,
		Log: log, Workers: func() int { return 2 },
	})
	if err := e.queue.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	e.scanner = library.New(library.Config{
		Roots: []string{e.lib}, CacheDir: e.cache, FFmpeg: ffmpeg, Prober: e.prober, Store: e.st,
		Log: log, Salt: func() string { return "salt" }, AfterScan: e.queue.AutoEnqueue,
	})
	return e
}

func (e *env) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.queue.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

func (e *env) waitIdle(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		states, err := e.st.CountJobsByState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		busy := false
		for _, s := range states {
			busy = busy || s.State == transcode.StateQueued || s.State == transcode.StateRunning
		}
		if !busy {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("jobs did not finish in time")
}

func (e *env) profile(t *testing.T, name string) db.Profile {
	t.Helper()
	profiles, err := e.st.ListProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no profile %q", name)
	return db.Profile{}
}

// renditions returns the renditions of a profile by fixture path.
func (e *env) renditions(t *testing.T, profileID int64) map[string]db.ListAllRenditionsRow {
	t.Helper()
	ctx := context.Background()
	rows, err := e.st.ListAllRenditions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]db.ListAllRenditionsRow{}
	for _, r := range rows {
		if r.ProfileID != profileID {
			continue
		}
		m, err := e.st.GetMedia(ctx, r.MediaID)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(e.lib, m.Path)
		out[filepath.ToSlash(rel)] = r
	}
	return out
}

func TestQueueEndToEnd(t *testing.T) {
	e := setup(t, 5)
	ctx := context.Background()
	e.run(t)
	if err := e.scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	e.waitIdle(t, 4*time.Minute)

	apple := e.profile(t, "High quality HEVC (Apple)")
	compat := e.profile(t, "Compatible 1080p H.264")
	jobs, err := e.st.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, j := range jobs {
		if j.State != transcode.StateDone || j.ProfileID != apple.ID {
			t.Errorf("job %d (%s, profile %d): %s %s", j.ID, j.MediaTitle, j.ProfileID, j.State, j.Error)
		}
	}
	// The default profile skips the direct file and the HEVC MP4, and encodes
	// only Hi10P and VP9.
	encoded := map[string]bool{testutil.Hi10P: true, testutil.VP9: true}
	want := []string{testutil.SlowStart, testutil.MKV, testutil.AC3, testutil.Hi10P, testutil.HEVCHev1, testutil.VP9}
	got := e.renditions(t, apple.ID)
	if len(got) != len(want) || len(jobs) != len(want) {
		t.Fatalf("Apple renditions = %d, jobs = %d, want %d", len(got), len(jobs), len(want))
	}
	for _, path := range want {
		r, ok := got[path]
		if !ok {
			t.Errorf("no Apple rendition for %s", path)
			continue
		}
		t.Run("apple/"+path, func(t *testing.T) { checkApple(t, e, path, r.Path, encoded[path]) })
	}

	// The H.264 fallback, queued by hand for every file but the direct one.
	media, err := e.st.ListMediaForScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range media {
		if !strings.HasSuffix(m.Path, testutil.Direct) {
			if _, err = e.queue.Enqueue(ctx, m.ID, compat.ID, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	e.waitIdle(t, 4*time.Minute)
	fallback := e.renditions(t, compat.ID)
	if len(fallback) != len(media)-1 {
		t.Fatalf("H.264 renditions = %d, want %d", len(fallback), len(media)-1)
	}
	for path, r := range fallback {
		m, getErr := e.st.GetMedia(ctx, r.MediaID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		t.Run("h264/"+path, func(t *testing.T) { checkCompat(t, e, m, r.Path) })
	}

	// A damaged rendition goes back into the queue at the next start.
	total := len(got) + len(fallback)
	if err = os.Truncate(got[testutil.MKV].Path, 10); err != nil {
		t.Fatal(err)
	}
	if err = e.queue.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if after, _ := e.st.ListAllRenditions(ctx); len(after) != total-1 {
		t.Fatalf("damaged rendition not removed: %d rows", len(after))
	}
	e.waitIdle(t, time.Minute)
	if again, _ := e.st.ListAllRenditions(ctx); len(again) != total {
		t.Fatalf("damaged rendition not made again: %d rows", len(again))
	}

	// A profile change marks the renditions stale; a re-transcode replaces them
	// and deletes the old files.
	old := e.renditions(t, apple.ID)
	params, _ := transcode.ParseParams(apple.ParamsJson)
	params.Quality++
	if err = e.queue.UpdateProfile(ctx, apple.ID, transcode.ProfileInput{
		Name: apple.Name, Policy: apple.AutoPolicy, Priority: apple.Priority, Params: params,
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := e.st.CountStaleRenditions(ctx)
	if err != nil || len(stale) != 1 || stale[0].N != int64(len(old)) {
		t.Fatalf("stale = %+v, %v", stale, err)
	}
	n, err := e.queue.RetranscodeStale(ctx, apple.ID)
	if err != nil || n != len(old) {
		t.Fatalf("retranscode = %d, %v", n, err)
	}
	e.waitIdle(t, 4*time.Minute)
	if now := e.renditions(t, apple.ID); len(now) != len(old) {
		t.Fatalf("renditions after re-transcode = %d, want %d", len(now), len(old))
	}
	if stale, _ := e.st.CountStaleRenditions(ctx); len(stale) != 0 {
		t.Fatalf("still stale: %+v", stale)
	}
	for _, r := range old {
		if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
			t.Errorf("replaced rendition file %s still exists", r.Path)
		}
	}
}

// probeOutput checks what every rendition shares and returns the probe.
func probeOutput(t *testing.T, e *env, path string) *probe.Result {
	t.Helper()
	res, _, err := e.prober.Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	for k := range res.Format.Tags {
		if strings.EqualFold(k, "title") {
			t.Fatal("metadata title not stripped")
		}
	}
	f, err := os.Open(path) //nolint:gosec // G304: test output
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	st, _ := f.Stat()
	if ok, err := probe.MoovFirst(f, st.Size()); err != nil || !ok {
		t.Fatalf("moov is not at the start (%v)", err)
	}
	return res
}

// checkKeyframes checks that an encoded video has a keyframe at least every two seconds.
func checkKeyframes(t *testing.T, path string) {
	t.Helper()
	out := testutil.Probe(t, path, "-select_streams", "v:0", "-skip_frame", "nokey",
		"-show_entries", "frame=pts_time", "-of", "csv=p=0")
	var prev float64
	for i, line := range strings.Fields(out) {
		ts, err := strconv.ParseFloat(strings.TrimSuffix(line, ","), 64)
		if err != nil {
			t.Fatalf("bad pts %q", line)
		}
		if i > 0 && ts-prev > 2.05 {
			t.Fatalf("keyframe gap %.2f s", ts-prev)
		}
		prev = ts
	}
}

func checkApple(t *testing.T, e *env, fixture, path string, encoded bool) {
	res := probeOutput(t, e, path)
	v, a := res.Video(), res.Audio("")
	switch {
	case v == nil:
		t.Fatal("no video")
	case encoded:
		if v.CodecName != "hevc" || v.CodecTagString != "hvc1" || v.BitDepth() != 10 {
			t.Fatalf("encoded video = %s/%s %d-bit, want 10-bit hvc1", v.CodecName, v.CodecTagString, v.BitDepth())
		}
		checkKeyframes(t, path)
	case fixture == testutil.HEVCHev1:
		if v.CodecName != "hevc" || v.CodecTagString != "hvc1" {
			t.Fatalf("copied HEVC = %s/%s, want the hvc1 tag", v.CodecName, v.CodecTagString)
		}
	default:
		if !probe.H264Compatible(v) {
			t.Fatalf("copied video = %+v, want the original H.264", v)
		}
	}
	if a == nil || a.CodecName != "aac" {
		t.Fatalf("audio = %+v", a)
	}
	if fixture == testutil.AC3 && a.Channels != 6 {
		t.Fatalf("5.1 audio became %d channels", a.Channels)
	}
}

func checkCompat(t *testing.T, e *env, m db.Medium, path string) {
	res := probeOutput(t, e, path)
	v := res.Video()
	if v == nil || !probe.H264Compatible(v) {
		t.Fatalf("video is not browser-compatible H.264: %+v", v)
	}
	if a := res.Audio(""); a == nil || a.CodecName != "aac" || a.Channels > 2 {
		t.Fatalf("audio = %+v", a)
	}
	if v.Height > 1080 {
		t.Fatalf("height %d above the profile limit", v.Height)
	}
	if m.Playability == string(probe.Transcode) || m.Playability == string(probe.Conditional) {
		checkKeyframes(t, path)
	}
	if m.Hdr == 1 && (v.ColorTransfer == "smpte2084" || v.ColorTransfer == "arib-std-b67") {
		t.Fatalf("HDR not tone mapped: transfer %s", v.ColorTransfer)
	}
}

func TestQueueCancel(t *testing.T) {
	e := setup(t, 20)
	ctx := context.Background()
	if err := e.scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	// Remove the queued automatic jobs; the test queues its own slow job.
	jobs, _ := e.st.ListJobs(ctx, 100)
	for _, j := range jobs {
		if err := e.queue.Cancel(ctx, j.ID); err != nil {
			t.Fatal(err)
		}
	}
	slow, err := e.queue.CreateProfile(ctx, transcode.ProfileInput{
		Name: "slow", Policy: transcode.PolicyManual,
		Params: transcode.Params{VideoMode: transcode.VideoEncode, Codec: "hevc", Encoder: "libx265",
			Quality: 20, Preset: "veryslow", AudioBitrateK: 128, Stereo: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var mediaID int64
	rows, _ := e.st.ListMediaForScan(ctx)
	for _, r := range rows {
		if strings.HasSuffix(r.Path, testutil.HDR) {
			mediaID = r.ID
		}
	}
	added, err := e.queue.Enqueue(ctx, mediaID, slow, 100)
	if err != nil || !added {
		t.Fatalf("enqueue = %v, %v", added, err)
	}
	e.run(t)

	var jobID int64
	deadline := time.Now().Add(30 * time.Second)
	for jobID == 0 && time.Now().Before(deadline) {
		js, _ := e.st.ListJobsForMedia(ctx, mediaID)
		for _, j := range js {
			if _, live := e.queue.Live(j.ID); live && j.State == transcode.StateRunning {
				jobID = j.ID
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if jobID == 0 {
		t.Fatal("job did not start")
	}
	time.Sleep(time.Second)
	if err = e.queue.Cancel(ctx, jobID); err != nil {
		t.Fatal(err)
	}
	e.waitIdle(t, 30*time.Second)
	j, err := e.st.GetJob(ctx, jobID)
	if err != nil || j.State != transcode.StateCanceled {
		t.Fatalf("job = %+v, %v", j, err)
	}
	partials, _ := filepath.Glob(filepath.Join(e.cache, "tmp", "*"))
	if len(partials) != 0 {
		t.Fatalf("partial files left: %v", partials)
	}
	if rs, _ := e.st.ListRenditionsForMedia(ctx, mediaID); len(rs) != 0 {
		t.Fatalf("canceled job left renditions: %+v", rs)
	}
}
