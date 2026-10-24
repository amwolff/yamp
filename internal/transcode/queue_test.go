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

func TestQueueEndToEnd(t *testing.T) {
	e := setup(t, 5)
	ctx := context.Background()
	e.run(t)
	if err := e.scanner.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	e.waitIdle(t, 4*time.Minute)

	jobs, err := e.st.ListJobs(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	// Every file except the direct one gets the when-needed profile.
	if len(jobs) != 7 {
		t.Fatalf("jobs = %d, want 7", len(jobs))
	}
	for _, j := range jobs {
		if j.State != transcode.StateDone {
			t.Errorf("job %d (%s): %s %s", j.ID, j.MediaTitle, j.State, j.Error)
		}
	}

	rows, err := e.st.ListAllRenditions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		m, getErr := e.st.GetMedia(ctx, r.MediaID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		t.Run(m.Title, func(t *testing.T) { checkRendition(t, e, m, r.Path) })
	}

	// A damaged rendition goes back into the queue at the next start.
	if err = os.Truncate(rows[0].Path, 10); err != nil {
		t.Fatal(err)
	}
	if err = e.queue.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := e.st.ListAllRenditions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(rows)-1 {
		t.Fatalf("damaged rendition not removed: %d rows", len(after))
	}
	e.waitIdle(t, time.Minute)
	if again, _ := e.st.ListAllRenditions(ctx); len(again) != len(rows) {
		t.Fatalf("damaged rendition not made again: %d rows", len(again))
	}

	// A profile change marks the renditions stale; a re-transcode replaces them
	// and deletes the old files.
	prof, err := e.st.GetProfile(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := transcode.ParseParams(prof.ParamsJson)
	params.Quality++
	if err = e.queue.UpdateProfile(ctx, prof.ID, transcode.ProfileInput{
		Name: prof.Name, Policy: prof.AutoPolicy, Priority: prof.Priority, Params: params,
	}); err != nil {
		t.Fatal(err)
	}
	stale, err := e.st.CountStaleRenditions(ctx)
	if err != nil || len(stale) != 1 || stale[0].N != int64(len(rows)) {
		t.Fatalf("stale = %+v, %v", stale, err)
	}
	n, err := e.queue.RetranscodeStale(ctx, prof.ID)
	if err != nil || n != len(rows) {
		t.Fatalf("retranscode = %d, %v", n, err)
	}
	e.waitIdle(t, 4*time.Minute)
	final, err := e.st.ListAllRenditions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != len(rows) {
		t.Fatalf("renditions after re-transcode = %d, want %d", len(final), len(rows))
	}
	if stale, _ := e.st.CountStaleRenditions(ctx); len(stale) != 0 {
		t.Fatalf("still stale: %+v", stale)
	}
	for _, r := range rows {
		if _, err := os.Stat(r.Path); !os.IsNotExist(err) {
			t.Errorf("replaced rendition file %s still exists", r.Path)
		}
	}
}

func checkRendition(t *testing.T, e *env, m db.Medium, path string) {
	res, _, err := e.prober.Probe(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	v := res.Video()
	if v == nil || !probe.H264Compatible(v) {
		t.Fatalf("video is not browser-compatible H.264: %+v", v)
	}
	if m.AudioCodec != "" {
		if a := res.Audio(""); a == nil || a.CodecName != "aac" || a.Channels > 2 {
			t.Fatalf("audio = %+v", a)
		}
	}
	if v.Height > 1080 {
		t.Fatalf("height %d above the profile limit", v.Height)
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
	if m.Playability == string(probe.Transcode) || m.Playability == string(probe.Conditional) {
		// Encoded video: keyframes at least every two seconds.
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
