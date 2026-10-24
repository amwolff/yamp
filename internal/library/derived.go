package library

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/amwolff/yamp/internal/fsutil"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/store/db"
)

// ThumbPath returns the thumbnail path of a media item.
func ThumbPath(cacheDir string, id int64) string {
	return filepath.Join(cacheDir, "thumbs", strconv.FormatInt(id, 10)+".jpg")
}

// thumbnail writes a JPEG from ~10% into the video. The fast attempt decodes
// only keyframes; short or long-GOP files may have none after the seek point,
// so the fallbacks decode every frame and finally take the first one.
func (s *Scanner) thumbnail(ctx context.Context, id int64, path string, videoIndex int, duration time.Duration) error {
	final := ThumbPath(s.cfg.CacheDir, id)
	tmp := final + ".partial"
	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	seek := fmt.Sprintf("%.3f", (duration / 10).Seconds())
	attempts := [][]string{
		{"-skip_frame", "nokey", "-ss", seek, "-i", path, "-vf", "thumbnail=24,scale=640:-2"},
		{"-ss", seek, "-i", path, "-vf", "thumbnail=24,scale=640:-2"},
		{"-i", path, "-vf", "scale=640:-2"},
	}
	var err error
	for _, a := range attempts {
		_ = fsutil.RemoveIfExists(tmp)
		args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}, a...)
		args = append(args, "-map", fmt.Sprintf("0:%d", videoIndex), "-frames:v", "1", "-q:v", "3",
			"-f", "image2", "-c:v", "mjpeg", "-update", "1", tmp)
		if err = run(ctx, s.cfg.FFmpeg, args...); err == nil && !fileEmpty(tmp) {
			break
		}
		if ctx.Err() != nil {
			break
		}
	}
	if fileEmpty(tmp) {
		_ = fsutil.RemoveIfExists(tmp)
		if err == nil {
			err = fmt.Errorf("ffmpeg wrote no thumbnail")
		}
		return err
	}
	if err := fsutil.Commit(tmp, final, s.cfg.Fsync); err != nil {
		return err
	}
	return s.cfg.Store.SetMediaThumb(ctx, db.SetMediaThumbParams{HasThumb: 1, ID: id})
}

// subtitles converts the text subtitle streams to WebVTT files in one ffmpeg
// run.
func (s *Scanner) subtitles(ctx context.Context, id int64, path string, res *probe.Result) error {
	subs := res.TextSubtitles()
	if len(subs) == 0 {
		return nil
	}
	dir := filepath.Join(s.cfg.CacheDir, "subs", strconv.FormatInt(id, 10))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y", "-i", path}
	for _, sub := range subs {
		args = append(args, "-map", fmt.Sprintf("0:%d", sub.Index), "-c:s", "webvtt", "-f", "webvtt",
			filepath.Join(dir, fmt.Sprintf("%d.vtt.partial", sub.Index)))
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	if err := run(ctx, s.cfg.FFmpeg, args...); err != nil {
		return err
	}
	for n, sub := range subs {
		final := filepath.Join(dir, fmt.Sprintf("%d.vtt", sub.Index))
		if err := fsutil.Commit(final+".partial", final, s.cfg.Fsync); err != nil {
			return err
		}
		label := sub.Title()
		if label == "" {
			label = sub.Lang()
		}
		if label == "" {
			label = fmt.Sprintf("Track %d", n+1)
		}
		if err := s.cfg.Store.InsertSubtitle(ctx, db.InsertSubtitleParams{
			MediaID: id, StreamIndex: int64(sub.Index), Lang: sub.Lang(), Label: label, Path: final,
		}); err != nil {
			return err
		}
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: ffmpeg with arguments that YAMP builds; no shell
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := bytes.TrimSpace(stderr.Bytes())
		if len(msg) > 2048 {
			msg = msg[len(msg)-2048:]
		}
		return fmt.Errorf("%s: %w: %s", filepath.Base(name), err, msg)
	}
	return nil
}

func fileEmpty(path string) bool {
	st, err := os.Stat(path)
	return err != nil || st.Size() == 0
}
