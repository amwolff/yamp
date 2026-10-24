package transcode

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/amwolff/yamp/internal/events"
	"github.com/amwolff/yamp/internal/fsutil"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
)

// Job states.
const (
	StateQueued   = "queued"
	StateRunning  = "running"
	StateDone     = "done"
	StateFailed   = "failed"
	StateCanceled = "canceled"
)

// Live phases of a running job.
const (
	PhaseEncode   = "encode"
	PhaseFinalize = "finalize"
)

// stderrTail is the amount of ffmpeg standard error kept for failed jobs.
const stderrTail = 16 << 10

// diskMargin is kept free in addition to the estimated output size.
const diskMargin = 256 << 20

// QueueConfig configures a Queue.
type QueueConfig struct {
	Store    *store.Store
	Caps     *Caps
	Prober   *probe.Prober
	FFmpeg   string
	CacheDir string
	Fsync    bool
	Hub      *events.Hub
	Log      *slog.Logger
	// Workers returns the number of concurrent jobs.
	Workers func() int
}

// Live is the in-memory state of a running job.
type Live struct {
	Progress float64
	Speed    float64
	Phase    string
	Encoder  string
}

// Queue runs transcode jobs from the jobs table.
type Queue struct {
	cfg  QueueConfig
	wake chan struct{}
	done chan struct{}

	mu      sync.Mutex
	running map[int64]*runningJob
	wg      sync.WaitGroup
}

type runningJob struct {
	profileID int64
	cancel    context.CancelFunc
	// userCancel is set when an admin cancels the job.
	userCancel bool
	live       Live
}

// NewQueue returns a Queue. Call Recover once and then Run.
func NewQueue(cfg QueueConfig) *Queue {
	return &Queue{
		cfg:     cfg,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}, 1),
		running: make(map[int64]*runningJob),
	}
}

// RenditionDir returns the directory for the renditions of a media item.
func RenditionDir(cacheDir string, mediaID int64) string {
	return filepath.Join(cacheDir, "renditions", strconv.FormatInt(mediaID, 10))
}

func (q *Queue) tmpDir() string { return filepath.Join(q.cfg.CacheDir, "tmp") }

// Recover runs at startup: it requeues interrupted jobs, removes partial files,
// and requeues renditions whose file is missing or truncated (no fsync).
func (q *Queue) Recover(ctx context.Context) error {
	if err := q.cfg.Store.RequeueRunningJobs(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(q.tmpDir()); err != nil {
		return err
	}
	if err := os.MkdirAll(q.tmpDir(), 0o750); err != nil {
		return err
	}
	rows, err := q.cfg.Store.ListAllRenditions(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		st, err := os.Stat(r.Path)
		if err == nil && st.Size() == r.Size {
			continue
		}
		q.cfg.Log.Warn("rendition is missing or damaged; queueing it again", "media", r.MediaID, "profile", r.ProfileID)
		_ = fsutil.RemoveIfExists(r.Path)
		if err := q.cfg.Store.DeleteRendition(ctx, r.ID); err != nil {
			return err
		}
		if _, err := q.Enqueue(ctx, r.MediaID, r.ProfileID, 0); err != nil {
			return err
		}
	}
	monthAgo := time.Now().AddDate(0, -1, 0).Unix()
	return q.cfg.Store.DeleteFinishedJobs(ctx, sql.NullInt64{Int64: monthAgo, Valid: true})
}

// Enqueue queues a job unless one is already queued or running for the same
// media and profile. It reports whether a job was added.
func (q *Queue) Enqueue(ctx context.Context, mediaID, profileID int64, priority int64) (bool, error) {
	n, err := q.cfg.Store.EnqueueJob(ctx, db.EnqueueJobParams{
		MediaID: mediaID, ProfileID: profileID, Priority: priority, CreatedAt: time.Now().Unix(),
	})
	if err != nil {
		return false, err
	}
	if n > 0 {
		q.notify()
	}
	return n > 0, nil
}

// AutoEnqueue queues the jobs that the profile policies select.
func (q *Queue) AutoEnqueue(ctx context.Context) {
	n, err := q.cfg.Store.AutoEnqueue(ctx, time.Now().Unix())
	if err != nil {
		q.cfg.Log.Error("auto enqueue failed", "err", err)
		return
	}
	if n > 0 {
		q.cfg.Log.Info("queued transcode jobs", "count", n)
		q.notify()
	}
}

// Cancel stops a running job or cancels a queued one.
func (q *Queue) Cancel(ctx context.Context, jobID int64) error {
	q.mu.Lock()
	if rj, ok := q.running[jobID]; ok {
		rj.userCancel = true
		rj.cancel()
		q.mu.Unlock()
		return nil
	}
	q.mu.Unlock()
	_, err := q.cfg.Store.CancelQueuedJob(ctx, db.CancelQueuedJobParams{
		FinishedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}, ID: jobID,
	})
	q.publish()
	return err
}

// Live returns the live state of a running job.
func (q *Queue) Live(jobID int64) (Live, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	rj, ok := q.running[jobID]
	if !ok {
		return Live{}, false
	}
	return rj.live, true
}

func (q *Queue) notify() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
	q.publish()
}

func (q *Queue) publish() {
	if q.cfg.Hub != nil {
		q.cfg.Hub.Publish(events.Jobs)
	}
}

func (q *Queue) active() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.running)
}

// Run starts jobs until ctx ends. Then it waits for the running jobs, which go
// back to the queue.
func (q *Queue) Run(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		q.dispatch(ctx)
		select {
		case <-ctx.Done():
			q.wg.Wait()
			return
		case <-q.wake:
		case <-q.done:
		case <-tick.C:
		}
	}
}

func (q *Queue) dispatch(ctx context.Context) {
	for ctx.Err() == nil && q.active() < q.cfg.Workers() {
		job, err := q.cfg.Store.ClaimJob(ctx, sql.NullInt64{Int64: time.Now().Unix(), Valid: true})
		if errors.Is(err, sql.ErrNoRows) {
			return
		}
		if err != nil {
			if ctx.Err() == nil {
				q.cfg.Log.Error("claim job failed", "err", err)
			}
			return
		}
		jctx, cancel := context.WithCancel(ctx)
		q.mu.Lock()
		q.running[job.ID] = &runningJob{profileID: job.ProfileID, cancel: cancel, live: Live{Phase: PhaseEncode}}
		q.mu.Unlock()
		q.wg.Add(1)
		go func() {
			defer q.wg.Done()
			defer cancel()
			q.finish(ctx, job.ID, q.runJob(jctx, job))
		}()
		q.publish()
	}
}

// finish records the result of a job. ctx is the queue context: if it ended,
// the job was interrupted by a shutdown and stays queued.
func (q *Queue) finish(ctx context.Context, jobID int64, runErr error) {
	q.mu.Lock()
	rj := q.running[jobID]
	delete(q.running, jobID)
	q.mu.Unlock()

	state, msg, progress := StateDone, "", 1.0
	switch {
	case runErr == nil:
	case rj != nil && rj.userCancel:
		state, msg, progress = StateCanceled, "", 0
	case ctx.Err() != nil:
		// Shutdown: Recover puts the job back into the queue at the next start.
		return
	default:
		state, msg, progress = StateFailed, runErr.Error(), 0
		q.cfg.Log.Error("transcode job failed", "job", jobID, "err", runErr)
	}
	// Use a fresh context: the job context may already be canceled.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := q.cfg.Store.FinishJob(fctx, db.FinishJobParams{
		State: state, Error: msg, Progress: progress,
		FinishedAt: sql.NullInt64{Int64: time.Now().Unix(), Valid: true}, ID: jobID,
	}); err != nil {
		q.cfg.Log.Error("record job result failed", "job", jobID, "err", err)
	}
	select {
	case q.done <- struct{}{}:
	default:
	}
	q.publish()
}

func (q *Queue) setLive(jobID int64, f func(*Live)) {
	q.mu.Lock()
	if rj, ok := q.running[jobID]; ok {
		f(&rj.live)
	}
	q.mu.Unlock()
}

func (q *Queue) runJob(ctx context.Context, job db.ClaimJobRow) error {
	st := q.cfg.Store
	m, err := st.GetMedia(ctx, job.MediaID)
	if err != nil {
		return fmt.Errorf("load media: %w", err)
	}
	if m.Status != "ok" {
		return errors.New("the source file is missing")
	}
	prof, err := st.GetProfile(ctx, job.ProfileID)
	if err != nil {
		return fmt.Errorf("load profile: %w", err)
	}
	params, err := ParseParams(prof.ParamsJson)
	if err != nil {
		return fmt.Errorf("profile %q: %w", prof.Name, err)
	}
	res, err := probe.Parse([]byte(m.ProbeJson))
	if err != nil {
		return err
	}
	encoder := ""
	if NeedsVideoEncode(res, params) {
		if encoder, err = q.cfg.Caps.Resolve(params.Codec, params.Encoder); err != nil {
			return err
		}
	}

	tmp := filepath.Join(q.tmpDir(), fmt.Sprintf("%d.partial", job.ID))
	final := filepath.Join(RenditionDir(q.cfg.CacheDir, m.ID),
		fmt.Sprintf("%d-%s-%d.mp4", prof.ID, prof.ParamsHash[:8], job.ID))
	plan, err := BuildArgs(Input{Path: m.Path, Output: tmp, Probe: res, Params: params, Encoder: encoder, Caps: q.cfg.Caps})
	if err != nil {
		return err
	}
	if err = q.checkDiskSpace(m, params, plan); err != nil {
		return err
	}
	q.setLive(job.ID, func(l *Live) { l.Encoder = plan.Encoder })
	q.cfg.Log.Info("transcode started", "job", job.ID, "media", m.Pid, "profile", prof.Name, "encoder", plan.Encoder)

	if err = q.runFFmpeg(ctx, job.ID, plan.Args, time.Duration(m.DurationMs)*time.Millisecond); err != nil {
		_ = fsutil.RemoveIfExists(tmp)
		return err
	}
	q.setLive(job.ID, func(l *Live) { l.Phase = PhaseFinalize; l.Progress = 1 })
	q.publish()

	if err = fsutil.Commit(tmp, final, q.cfg.Fsync); err != nil {
		_ = fsutil.RemoveIfExists(tmp)
		return err
	}
	out, _, err := q.cfg.Prober.Probe(ctx, final)
	if err != nil {
		_ = fsutil.RemoveIfExists(final)
		return fmt.Errorf("probe output: %w", err)
	}
	ov, oa := out.Video(), out.Audio("")
	if ov == nil {
		_ = fsutil.RemoveIfExists(final)
		return errors.New("the output has no video stream")
	}
	info, err := os.Stat(final)
	if err != nil {
		return err
	}
	var replaced []db.ListReplacedRenditionsRow
	err = st.Tx(ctx, func(tq *db.Queries) error {
		id, insErr := tq.InsertRendition(ctx, db.InsertRenditionParams{
			MediaID: m.ID, ProfileID: prof.ID, ParamsHash: params.Hash(), Path: final, Size: info.Size(),
			Mime: "video/mp4", Codecs: probe.JoinCodecs(false, ov, oa),
			Width: int64(ov.Width), Height: int64(ov.Height), CreatedAt: time.Now().Unix(),
		})
		if insErr != nil {
			return insErr
		}
		var listErr error
		replaced, listErr = tq.ListReplacedRenditions(ctx, db.ListReplacedRenditionsParams{MediaID: m.ID, ProfileID: prof.ID, ID: id})
		if listErr != nil {
			return listErr
		}
		for _, r := range replaced {
			if delErr := tq.DeleteRendition(ctx, r.ID); delErr != nil {
				return delErr
			}
		}
		return nil
	})
	if err != nil {
		_ = fsutil.RemoveIfExists(final)
		return err
	}
	for _, r := range replaced {
		if err := fsutil.RemoveIfExists(r.Path); err != nil {
			q.cfg.Log.Warn("remove replaced rendition failed", "err", err)
		}
	}
	q.cfg.Log.Info("transcode finished", "job", job.ID, "media", m.Pid, "size", info.Size())
	return nil
}

func (q *Queue) runFFmpeg(ctx context.Context, jobID int64, args []string, duration time.Duration) error {
	cmd := exec.CommandContext(ctx, q.cfg.FFmpeg, args...) //nolint:gosec // G204: ffmpeg with arguments from BuildArgs; no shell
	// Ask ffmpeg to stop cleanly first; kill it if it does not exit in time.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tailBuffer{max: stderrTail}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	var (
		lastDB, lastEvent time.Time
		parseErr          = make(chan error, 1)
	)
	go func() {
		err := ParseProgress(stdout, func(p Progress) {
			frac := 0.0
			if duration > 0 {
				frac = min(float64(p.OutTime)/float64(duration), 1)
			}
			q.setLive(jobID, func(l *Live) { l.Progress, l.Speed = frac, p.Speed })
			now := time.Now()
			if now.Sub(lastEvent) >= time.Second {
				lastEvent = now
				q.publish()
			}
			if now.Sub(lastDB) >= 5*time.Second {
				lastDB = now
				_ = q.cfg.Store.UpdateJobProgress(ctx, db.UpdateJobProgressParams{Progress: frac, ID: jobID})
			}
		})
		// Drain the pipe before reporting: Wait must not run while it is read.
		_, _ = io.Copy(io.Discard, stdout)
		parseErr <- err
	}()
	if err := <-parseErr; err != nil {
		// Progress is only for display; the job continues.
		q.cfg.Log.Warn("read ffmpeg progress", "job", jobID, "err", err)
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ffmpeg: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

func (q *Queue) checkDiskSpace(m db.Medium, p Params, plan Plan) error {
	free, ok := FreeBytes(q.cfg.CacheDir)
	if !ok {
		return nil
	}
	need := estimateSize(m, p, plan) + diskMargin
	if free < need {
		return fmt.Errorf("not enough disk space in the cache directory: %d MiB free, %d MiB needed", free>>20, need>>20)
	}
	return nil
}

// estimateSize guesses the output size, doubled because faststart rewrites the
// file at the end.
func estimateSize(m db.Medium, p Params, plan Plan) int64 {
	seconds := m.DurationMs / 1000
	var est int64
	switch {
	case plan.CopyVideo:
		est = m.Size
	case p.MaxBitrateK > 0:
		est = int64(p.MaxBitrateK+p.AudioBitrateK) * 1000 / 8 * seconds
	default:
		est = min(m.Size, 12_000_000/8*seconds)
	}
	return 2 * est
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}
