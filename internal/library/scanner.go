// Package library scans the library roots, probes new files and keeps the media
// table up to date.
package library

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/amwolff/yamp/internal/events"
	"github.com/amwolff/yamp/internal/fsutil"
	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
)

var videoExts = map[string]bool{
	".mp4": true, ".m4v": true, ".mov": true, ".mkv": true, ".webm": true, ".avi": true,
	".ts": true, ".m2ts": true, ".mts": true, ".wmv": true, ".flv": true, ".mpg": true, ".mpeg": true,
}

// Status is the state of the current or last scan.
type Status struct {
	Running   bool
	Started   time.Time
	Finished  time.Time
	Total     int
	Done      int
	Added     int
	Updated   int
	Missing   int
	Issues    []string
	LastError string
}

// Config configures a Scanner.
type Config struct {
	Roots    []string
	CacheDir string
	FFmpeg   string
	Fsync    bool
	Prober   *probe.Prober
	Store    *store.Store
	Hub      *events.Hub
	Log      *slog.Logger
	// Tonemap enables tone mapping of HDR thumbnails.
	Tonemap bool
	// Salt returns the current salt for public IDs.
	Salt func() string
	// AfterScan runs after each completed scan, for example to queue jobs.
	AfterScan func(context.Context)
}

// Scanner indexes the library. Only one scan runs at a time.
type Scanner struct {
	cfg     Config
	trigger chan struct{}

	mu     sync.Mutex
	status Status
}

// New returns a Scanner.
func New(cfg Config) *Scanner {
	return &Scanner{cfg: cfg, trigger: make(chan struct{}, 1)}
}

// Status returns a copy of the scan status.
func (s *Scanner) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	st.Issues = append([]string(nil), s.status.Issues...)
	return st
}

// Trigger requests a scan. It returns false if a scan is already pending.
func (s *Scanner) Trigger() bool {
	select {
	case s.trigger <- struct{}{}:
		return true
	default:
		return false
	}
}

// Run scans on Trigger and, if interval() > 0, periodically, until ctx ends.
func (s *Scanner) Run(ctx context.Context, interval func() time.Duration) {
	for {
		var (
			tick  <-chan time.Time
			timer *time.Timer
		)
		if d := interval(); d > 0 {
			timer = time.NewTimer(d)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
		case <-s.trigger:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
		if ctx.Err() != nil {
			return
		}
		if err := s.Scan(ctx); err != nil && ctx.Err() == nil {
			s.cfg.Log.Error("library scan failed", "err", err)
		}
	}
}

type file struct {
	path, root, relDir string
	size, mtime        int64
}

// Scan runs one full scan.
func (s *Scanner) Scan(ctx context.Context) error {
	s.mu.Lock()
	if s.status.Running {
		s.mu.Unlock()
		return errors.New("a scan is already running")
	}
	s.status = Status{Running: true, Started: time.Now()}
	s.mu.Unlock()
	s.publish()

	err := s.scan(ctx)

	s.mu.Lock()
	s.status.Running = false
	s.status.Finished = time.Now()
	if err != nil {
		s.status.LastError = err.Error()
	}
	s.mu.Unlock()
	s.publish()
	if err == nil && s.cfg.AfterScan != nil {
		s.cfg.AfterScan(ctx)
	}
	return err
}

func (s *Scanner) publish() {
	if s.cfg.Hub != nil {
		s.cfg.Hub.Publish(events.Scan)
	}
}

func (s *Scanner) update(f func(*Status)) {
	s.mu.Lock()
	f(&s.status)
	s.mu.Unlock()
}

func (s *Scanner) issue(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	s.cfg.Log.Warn("library scan issue", "issue", msg)
	s.update(func(st *Status) { st.Issues = append(st.Issues, msg) })
}

func (s *Scanner) walk() ([]file, error) {
	var files []file
	for _, root := range s.cfg.Roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				s.issue("cannot read %s: %v", path, err)
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") && path != root {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() || !videoExts[strings.ToLower(filepath.Ext(path))] {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				s.issue("cannot stat %s: %v", path, err)
				return nil
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			if rel == "." {
				rel = ""
			}
			files = append(files, file{
				path: path, root: root, relDir: filepath.ToSlash(rel),
				size: info.Size(), mtime: info.ModTime().UnixNano(),
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func (s *Scanner) scan(ctx context.Context) error {
	files, err := s.walk()
	if err != nil {
		return err
	}
	s.update(func(st *Status) { st.Total = len(files) })

	rows, err := s.cfg.Store.ListMediaForScan(ctx)
	if err != nil {
		return err
	}
	byPath := make(map[string]db.ListMediaForScanRow, len(rows))
	byFP := make(map[string]db.ListMediaForScanRow, len(rows))
	for _, r := range rows {
		byPath[r.Path] = r
		byFP[hex.EncodeToString(r.Fingerprint)] = r
	}
	onDisk := make(map[string]bool, len(files))
	for _, f := range files {
		onDisk[f.path] = true
	}
	seen := make(map[int64]bool, len(rows))
	salt := s.cfg.Salt()
	lastPublish := time.Now()

	for i, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.scanFile(ctx, f, salt, byPath, byFP, onDisk, seen); err != nil {
			s.issue("%s: %v", f.path, err)
		}
		s.update(func(st *Status) { st.Done = i + 1 })
		if time.Since(lastPublish) > time.Second {
			s.publish()
			lastPublish = time.Now()
		}
	}

	now := time.Now().Unix()
	for _, r := range rows {
		if !seen[r.ID] && r.Status == "ok" {
			if err := s.cfg.Store.SetMediaStatus(ctx, db.SetMediaStatusParams{Status: "missing", UpdatedAt: now, ID: r.ID}); err != nil {
				return err
			}
			s.update(func(st *Status) { st.Missing++ })
		}
	}
	return nil
}

func (s *Scanner) scanFile(ctx context.Context, f file, salt string,
	byPath, byFP map[string]db.ListMediaForScanRow, onDisk map[string]bool, seen map[int64]bool,
) error {
	now := time.Now().Unix()
	row, known := byPath[f.path]
	if known && row.Size == f.size && row.MtimeNs == f.mtime {
		seen[row.ID] = true
		if row.Status != "ok" {
			if err := s.cfg.Store.SetMediaStatus(ctx, db.SetMediaStatusParams{Status: "ok", UpdatedAt: now, ID: row.ID}); err != nil {
				return err
			}
		}
		if row.HasThumb == 0 {
			return s.thumbnailFor(ctx, row.ID)
		}
		return nil
	}

	fp, size, err := Fingerprint(f.path)
	if err != nil {
		return err
	}
	f.size = size
	location := db.UpdateMediaLocationParams{
		Path: f.path, Root: f.root, RelDir: f.relDir, Title: Title(f.path),
		Size: f.size, MtimeNs: f.mtime, UpdatedAt: now,
	}
	if other, ok := byFP[hex.EncodeToString(fp)]; ok {
		if other.Path != f.path && onDisk[other.Path] {
			return fmt.Errorf("same content as %s; ignored", other.Path)
		}
		// The same content: the file was renamed, moved or only touched.
		location.ID = other.ID
		if err = s.cfg.Store.UpdateMediaLocation(ctx, location); err != nil {
			return err
		}
		seen[other.ID] = true
		delete(byPath, other.Path)
		byPath[f.path] = other
		s.update(func(st *Status) { st.Updated++ })
		return nil
	}

	res, raw, err := s.cfg.Prober.Probe(ctx, f.path)
	if err != nil {
		return err
	}
	moovFirst := false
	if c := probe.ContainerOf(res, f.path); c == probe.ContainerMP4 || c == probe.ContainerMOV {
		if moovFirst, err = moovFirstFile(f.path); err != nil {
			return err
		}
	}
	a, err := probe.Classify(res, f.path, moovFirst)
	if err != nil {
		return err
	}
	audioCodec := ""
	if a.Audio != nil {
		audioCodec = a.Audio.CodecName
	}
	pid := privacy.PID(salt, fp)
	w, h := a.Video.DisplaySize()
	var id int64
	if known {
		// The content at this path changed: drop the old derived files.
		id = row.ID
		if err = s.dropDerived(ctx, id); err != nil {
			return err
		}
		err = s.cfg.Store.UpdateMediaContent(ctx, db.UpdateMediaContentParams{
			Path: f.path, Root: f.root, RelDir: f.relDir, Title: Title(f.path), Size: f.size, MtimeNs: f.mtime,
			Fingerprint: fp, Pid: pid, DurationMs: res.Duration().Milliseconds(), Container: a.Container,
			Width: int64(w), Height: int64(h), VideoCodec: a.Video.CodecName,
			AudioCodec: audioCodec, Mime: a.Mime, Codecs: a.Codecs, Hdr: boolInt(a.HDR),
			Playability: string(a.Class), ProbeJson: string(raw), UpdatedAt: now, ID: id,
		})
		s.update(func(st *Status) { st.Updated++ })
	} else {
		id, err = s.cfg.Store.InsertMedia(ctx, db.InsertMediaParams{
			Path: f.path, Root: f.root, RelDir: f.relDir, Title: Title(f.path), Size: f.size, MtimeNs: f.mtime,
			Fingerprint: fp, Pid: pid, DurationMs: res.Duration().Milliseconds(), Container: a.Container,
			Width: int64(w), Height: int64(h), VideoCodec: a.Video.CodecName,
			AudioCodec: audioCodec, Mime: a.Mime, Codecs: a.Codecs, Hdr: boolInt(a.HDR),
			Playability: string(a.Class), ProbeJson: string(raw), AddedAt: now, UpdatedAt: now,
		})
		s.update(func(st *Status) { st.Added++ })
	}
	if err != nil {
		return err
	}
	seen[id] = true
	byFP[hex.EncodeToString(fp)] = db.ListMediaForScanRow{ID: id, Path: f.path, Fingerprint: fp, Status: "ok"}

	if err := s.thumbnail(ctx, id, f.path, a.Video, res.Duration()); err != nil {
		s.issue("%s: thumbnail: %v", f.path, err)
	}
	if err := s.subtitles(ctx, id, f.path, res); err != nil {
		s.issue("%s: subtitles: %v", f.path, err)
	}
	return nil
}

// dropDerived deletes the renditions and subtitles of a media item.
func (s *Scanner) dropDerived(ctx context.Context, id int64) error {
	paths, err := s.cfg.Store.ListRenditionPathsForMedia(ctx, id)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := fsutil.RemoveIfExists(p); err != nil {
			return err
		}
	}
	if err := s.cfg.Store.DeleteRenditionsForMedia(ctx, id); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.cfg.CacheDir, "subs", fmt.Sprint(id))); err != nil {
		return err
	}
	return s.cfg.Store.DeleteSubtitles(ctx, id)
}

func (s *Scanner) thumbnailFor(ctx context.Context, id int64) error {
	m, err := s.cfg.Store.GetMedia(ctx, id)
	if err != nil {
		return err
	}
	res, err := probe.Parse([]byte(m.ProbeJson))
	if err != nil {
		return err
	}
	v := res.Video()
	if v == nil {
		return probe.ErrNoVideo
	}
	return s.thumbnail(ctx, id, m.Path, v, res.Duration())
}

func moovFirstFile(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a file found under a library root
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return false, err
	}
	return probe.MoovFirst(f, st.Size())
}

// Title derives a display title from a file name.
func Title(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if !strings.Contains(name, " ") {
		name = strings.NewReplacer(".", " ", "_", " ").Replace(name)
	}
	if t := strings.TrimSpace(name); t != "" {
		return t
	}
	return filepath.Base(path)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// RotateSalt sets a new salt and re-derives every public ID in one transaction.
func RotateSalt(ctx context.Context, st *store.Store, salt string) error {
	if strings.TrimSpace(salt) == "" {
		return errors.New("the salt must not be empty")
	}
	return st.Tx(ctx, func(q *db.Queries) error {
		rows, err := q.ListMediaFingerprints(ctx)
		if err != nil {
			return err
		}
		// Park every pid first so that UNIQUE does not trip mid-update.
		for _, r := range rows {
			if err := q.UpdateMediaPID(ctx, db.UpdateMediaPIDParams{Pid: fmt.Sprintf("tmp-%d", r.ID), ID: r.ID}); err != nil {
				return err
			}
		}
		for _, r := range rows {
			if err := q.UpdateMediaPID(ctx, db.UpdateMediaPIDParams{Pid: privacy.PID(salt, r.Fingerprint), ID: r.ID}); err != nil {
				return err
			}
		}
		return q.SetSetting(ctx, db.SetSettingParams{Key: "salt", Value: salt})
	})
}
