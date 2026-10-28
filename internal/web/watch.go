package web

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/amwolff/yamp/internal/auth"
	"github.com/amwolff/yamp/internal/library"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/store/db"
	"github.com/amwolff/yamp/internal/transcode"
)

type source struct {
	URL   string
	Type  string
	Label string
}

type watchData struct {
	Media      db.Medium
	Sources    []source
	Subtitles  []db.Subtitle
	Renditions []db.ListRenditionsForMediaRow
	Unplayable transcodeForm
	Transcode  transcodeForm
	ResumeMs   int64
	Rating     ratingData
	Views      int64
	Status     statusData
}

type transcodeForm struct {
	ID       string
	Pid      string
	Profiles []db.Profile
	Selected int64
}

type ratingData struct {
	PID   string
	Stars int64
}

type statusData struct {
	PID string
	// Active is the newest queued or running job, if any.
	Active *jobView
	// Last is the newest finished job when there is no active one.
	Last *jobView
	// Polling is true when the fragment refreshes itself.
	Polling bool
	// Ready is set when a job finished while the page was open.
	Ready bool
}

type jobView struct {
	ID       int64
	State    string
	Profile  string
	Progress float64
	Phase    string
	Speed    float64
	Encoder  string
	Error    string
}

func (s *Server) mediaByPID(w http.ResponseWriter, r *http.Request) (db.Medium, bool) {
	m, err := s.Store.GetMediaByPID(r.Context(), r.PathValue("pid"))
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return m, false
	}
	if err != nil {
		s.serverError(w, r, err)
		return m, false
	}
	return m, true
}

func (s *Server) handleWatch(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	u := auth.UserFrom(ctx)
	d := watchData{Media: m, Rating: ratingData{PID: m.Pid}}
	var err error
	if d.Renditions, err = s.Store.ListRenditionsForMedia(ctx, m.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	// The original comes first when browsers can play it, then the renditions:
	// fresh ones before stale ones, by profile priority.
	if m.Mime != "" {
		label := "Original"
		if m.Playability == string(probe.Conditional) {
			label = "Original (" + m.VideoCodec + ")"
		}
		d.Sources = append(d.Sources, source{
			URL: "/media/" + m.Pid + "/original", Type: probe.TypeAttr(m.Mime, m.Codecs), Label: label,
		})
	}
	for _, rd := range d.Renditions {
		label := rd.ProfileName
		if rd.Stale {
			label += " (stale)"
		}
		d.Sources = append(d.Sources, source{
			URL:   fmt.Sprintf("/media/%s/r/%d", m.Pid, rd.ID),
			Type:  probe.TypeAttr(rd.Mime, rd.Codecs),
			Label: label,
		})
	}
	if d.Subtitles, err = s.Store.ListSubtitles(ctx, m.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	profiles, err := s.Store.ListProfiles(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Unplayable = transcodeForm{ID: "unplayable", Pid: m.Pid, Profiles: profiles, Selected: fallbackProfile(profiles)}
	d.Transcode = transcodeForm{ID: "transcode", Pid: m.Pid, Profiles: profiles}
	pb, err := s.Store.GetPlayback(ctx, db.GetPlaybackParams{UserID: u.ID, MediaID: m.ID})
	switch {
	case err == nil:
		if pb.Completed == 0 {
			d.ResumeMs = pb.PositionMs
		}
	case !errors.Is(err, sql.ErrNoRows):
		s.serverError(w, r, err)
		return
	}
	if d.Rating.Stars, err = s.Store.GetRating(ctx, db.GetRatingParams{UserID: u.ID, MediaID: m.ID}); err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.serverError(w, r, err)
		return
	}
	if d.Views, err = s.Store.CountViews(ctx, m.ID); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Status, err = s.status(r, m, false); err != nil {
		s.serverError(w, r, err)
		return
	}
	// The title holds only the public ID; the name is in the page body.
	s.render(w, r, http.StatusOK, "watch", s.newPage(r, m.Pid+" - YAMP", "watch", d))
}

// fallbackProfile returns the highest-priority H.264 profile, or 0.
func fallbackProfile(profiles []db.Profile) int64 {
	for _, p := range profiles {
		if params, err := transcode.ParseParams(p.ParamsJson); err == nil && params.Codec == "h264" {
			return p.ID
		}
	}
	return 0
}

func (s *Server) status(r *http.Request, m db.Medium, polling bool) (statusData, error) {
	sd := statusData{PID: m.Pid}
	jobs, err := s.Store.ListJobsForMedia(r.Context(), m.ID)
	if err != nil {
		return sd, err
	}
	for _, j := range jobs {
		v := &jobView{ID: j.ID, State: j.State, Profile: j.ProfileName, Progress: j.Progress, Error: j.Error}
		if live, ok := s.Queue.Live(j.ID); ok {
			v.Progress, v.Phase, v.Speed, v.Encoder = live.Progress, live.Phase, live.Speed, live.Encoder
		}
		if j.State == transcode.StateQueued || j.State == transcode.StateRunning {
			sd.Active = v
			break
		}
		if sd.Last == nil {
			sd.Last = v
		}
	}
	sd.Polling = sd.Active != nil
	if polling && sd.Active == nil && sd.Last != nil && sd.Last.State == transcode.StateDone {
		sd.Ready = true
	}
	return sd, nil
}

func (s *Server) handleWatchStatus(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	sd, err := s.status(r, m, true)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderTemplate(w, r, http.StatusOK, "watch", "watch-status", sd)
}

// serveFile serves a file with Range support, named after the public ID.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, path, mime, name, cache string) {
	f, err := os.Open(path) //nolint:gosec // G304: the path comes from the database, never from the request
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "file not found", http.StatusNotFound)
			return
		}
		s.serverError(w, r, err)
		return
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("Cache-Control", cache)
	h.Set("Content-Disposition", `inline; filename="`+name+`"`)
	http.ServeContent(w, r, "", st.ModTime(), f)
}

func extFor(mime string) string {
	if mime == "video/webm" {
		return ".webm"
	}
	return ".mp4"
}

func (s *Server) handleOriginal(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	if m.Mime == "" {
		http.Error(w, "the original file cannot play in a browser", http.StatusNotFound)
		return
	}
	s.serveFile(w, r, m.Path, m.Mime, m.Pid+extFor(m.Mime), "private, max-age=3600")
}

func (s *Server) handleRendition(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	rid, err := strconv.ParseInt(r.PathValue("rid"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	rd, err := s.Store.GetRendition(r.Context(), db.GetRenditionParams{ID: rid, MediaID: m.ID})
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.serveFile(w, r, rd.Path, rd.Mime, fmt.Sprintf("%s-%d.mp4", m.Pid, rd.ID), "private, max-age=3600")
}

func (s *Server) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	sid, err := strconv.ParseInt(strings.TrimSuffix(r.PathValue("sid"), ".vtt"), 10, 64)
	if err != nil {
		s.notFound(w, r)
		return
	}
	sub, err := s.Store.GetSubtitle(r.Context(), db.GetSubtitleParams{ID: sid, MediaID: m.ID})
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.serveFile(w, r, sub.Path, "text/vtt; charset=utf-8", fmt.Sprintf("%s-%d.vtt", m.Pid, sub.ID), "private, max-age=3600")
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	if m.HasThumb == 0 {
		http.Error(w, "no thumbnail", http.StatusNotFound)
		return
	}
	s.serveFile(w, r, library.ThumbPath(s.Config.CacheDir, m.ID), "image/jpeg", m.Pid+".jpg", "private, max-age=86400")
}

// handlePlayback records the resume position and the watched time. The player
// sends it every 10 s while playing, on pause, and on page hide.
func (s *Server) handlePlayback(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetMediaByPID(r.Context(), r.PathValue("pid"))
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	u := auth.UserFrom(r.Context())
	if err = r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	playKey := r.PostForm.Get("play")
	if playKey == "" || len(playKey) > 64 {
		http.Error(w, "bad play key", http.StatusBadRequest)
		return
	}
	pos, _ := strconv.ParseInt(r.PostForm.Get("pos"), 10, 64)
	watched, _ := strconv.ParseInt(r.PostForm.Get("watched"), 10, 64)
	pos = min(max(pos, 0), max(m.DurationMs, 0))
	// Heartbeats come every 10 s while playing, so cap a report at 30 s.
	watched = min(max(watched, 0), 30_000)
	completed := r.PostForm.Get("ended") == "1" || (m.DurationMs > 0 && pos >= m.DurationMs*95/100)
	now := time.Now().Unix()

	if err = s.Store.UpsertPlayback(r.Context(), db.UpsertPlaybackParams{
		UserID: u.ID, MediaID: m.ID, PositionMs: pos, Completed: boolInt(completed), UpdatedAt: now,
	}); err != nil {
		s.serverError(w, r, err)
		return
	}
	ev, err := s.Store.AddPlayEvent(r.Context(), db.AddPlayEventParams{
		PlayKey: fmt.Sprintf("%d:%s", u.ID, playKey), UserID: u.ID, MediaID: m.ID,
		StartedAt: now, UpdatedAt: now, WatchedMs: watched,
	})
	if errors.Is(err, sql.ErrNoRows) {
		// The key belongs to another user or video: ignore it.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// A play counts as a view after min(60 s, 25 % of the duration).
	threshold := int64(60_000)
	if m.DurationMs > 0 {
		threshold = min(threshold, m.DurationMs/4)
	}
	if ev.Counted == 0 && ev.WatchedMs >= threshold {
		if err := s.Store.MarkPlayEventCounted(r.Context(), ev.ID); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRating(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	u := auth.UserFrom(r.Context())
	stars, err := strconv.ParseInt(r.PostFormValue("stars"), 10, 64)
	if err != nil || stars < 0 || stars > 5 {
		http.Error(w, "stars must be from 0 to 5", http.StatusBadRequest)
		return
	}
	if stars == 0 {
		err = s.Store.DeleteRating(r.Context(), db.DeleteRatingParams{UserID: u.ID, MediaID: m.ID})
	} else {
		err = s.Store.UpsertRating(r.Context(), db.UpsertRatingParams{
			UserID: u.ID, MediaID: m.ID, Stars: stars, UpdatedAt: time.Now().Unix(),
		})
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if r.Header.Get("HX-Request") != "" {
		s.renderTemplate(w, r, http.StatusOK, "watch", "rating", ratingData{PID: m.Pid, Stars: stars})
		return
	}
	http.Redirect(w, r, "/v/"+m.Pid, http.StatusSeeOther)
}

func (s *Server) handleMediaTranscode(w http.ResponseWriter, r *http.Request) {
	m, ok := s.mediaByPID(w, r)
	if !ok {
		return
	}
	pid, err := strconv.ParseInt(r.PostFormValue("profile"), 10, 64)
	if err != nil {
		http.Error(w, "bad profile", http.StatusBadRequest)
		return
	}
	if _, err = s.Store.GetProfile(r.Context(), pid); err != nil {
		http.Error(w, "unknown profile", http.StatusBadRequest)
		return
	}
	added, err := s.Queue.Enqueue(r.Context(), m.ID, pid, 100)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	msg := "The transcode job is in the queue."
	if !added {
		msg = "A job for this profile is already in the queue."
	}
	s.redirectFlash(w, r, "/v/"+m.Pid, msg)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
