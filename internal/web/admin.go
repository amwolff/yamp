package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/amwolff/yamp/internal/auth"
	"github.com/amwolff/yamp/internal/events"
	"github.com/amwolff/yamp/internal/library"
	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/settings"
	"github.com/amwolff/yamp/internal/store/db"
	"github.com/amwolff/yamp/internal/transcode"
)

// ---- Library ----

type libraryData struct {
	Roots        []string
	Classes      []db.CountMediaByPlayabilityRow
	Missing      int64
	Scan         library.Status
	ScanInterval int
}

func (s *Server) handleAdminLibrary(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := libraryData{
		Roots: s.Config.Libraries, Scan: s.Scanner.Status(),
		ScanInterval: int(s.Settings.ScanInterval() / time.Minute),
	}
	var err error
	if d.Classes, err = s.Store.CountMediaByPlayability(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	if d.Missing, err = s.Store.CountMissingMedia(ctx); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_library", s.newPage(r, "Admin - YAMP", "library", d))
}

func (s *Server) handleScanStatus(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, r, http.StatusOK, "admin_library", "scan-status", s.Scanner.Status())
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	msg := "The scan started."
	if !s.Scanner.Trigger() {
		msg = "A scan is already pending."
	}
	s.redirectFlash(w, r, "/admin/library", msg)
}

func (s *Server) handleScanInterval(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PostFormValue("minutes"))
	if err != nil || n < 0 || n > 7*24*60 {
		s.redirectFlash(w, r, "/admin/library", "The interval must be from 0 to 10080 minutes.")
		return
	}
	if err := s.Settings.Set(r.Context(), settings.KeyScanInterval, strconv.Itoa(n)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectFlash(w, r, "/admin/library", "Saved. The new interval applies after the next scan.")
}

// ---- Transcoding ----

type profileView struct {
	db.Profile
	Params transcode.Params
	Stale  int64
}

type transcodingData struct {
	Caps     *transcode.Caps
	Workers  int
	Encoders []string
	Policies []string
	Codecs   []string
	Presets  []string
	Heights  []int
	Forms    []profileForm
	New      profileForm
}

// profileForm is one profile editor; Opts carries the select options.
type profileForm struct {
	P    profileView
	Opts *transcodingData
}

func (s *Server) handleAdminTranscoding(w http.ResponseWriter, r *http.Request) {
	d, err := s.transcodingData(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.link()
	s.render(w, r, http.StatusOK, "admin_transcoding", s.newPage(r, "Admin - YAMP", "transcoding", d))
}

// link points every form at the shared options. It runs after the struct has
// its final address.
func (d *transcodingData) link() {
	for i := range d.Forms {
		d.Forms[i].Opts = d
	}
	d.New.Opts = d
}

func (s *Server) transcodingData(ctx context.Context) (*transcodingData, error) {
	d := &transcodingData{
		Caps: s.Caps, Workers: s.Settings.Workers(),
		Encoders: append([]string{"auto"}, sortedEncoders()...),
		Policies: transcode.Policies, Codecs: transcode.Codecs,
		Presets: []string{"", "veryfast", "faster", "fast", "medium", "slow", "slower", "veryslow", "4", "6", "8", "10"},
		Heights: []int{0, 480, 720, 1080, 1440, 2160},
	}
	profiles, err := s.Store.ListProfiles(ctx)
	if err != nil {
		return d, err
	}
	stale, err := s.Store.CountStaleRenditions(ctx)
	if err != nil {
		return d, err
	}
	for _, p := range profiles {
		params, _ := transcode.ParseParams(p.ParamsJson)
		pv := profileView{Profile: p, Params: params}
		for _, st := range stale {
			if st.ProfileID == p.ID {
				pv.Stale = st.N
			}
		}
		d.Forms = append(d.Forms, profileForm{P: pv})
	}
	d.New = profileForm{P: profileView{
		Profile: db.Profile{AutoPolicy: transcode.PolicyManual},
		Params:  transcode.Presets[0].Params,
	}}
	return d, nil
}

func sortedEncoders() []string {
	return []string{"libx264", "h264_qsv", "h264_vaapi", "libx265", "hevc_qsv", "hevc_vaapi", "libsvtav1"}
}

func profileInput(r *http.Request) transcode.ProfileInput {
	atoi := func(k string) int { n, _ := strconv.Atoi(strings.TrimSpace(r.PostFormValue(k))); return n }
	prio, _ := strconv.ParseInt(r.PostFormValue("priority"), 10, 64)
	return transcode.ProfileInput{
		Name:     r.PostFormValue("name"),
		Policy:   r.PostFormValue("policy"),
		Priority: prio,
		Params: transcode.Params{
			VideoMode:     r.PostFormValue("video_mode"),
			Codec:         r.PostFormValue("codec"),
			Encoder:       r.PostFormValue("encoder"),
			Quality:       atoi("quality"),
			MaxBitrateK:   atoi("max_bitrate_k"),
			Preset:        strings.TrimSpace(r.PostFormValue("preset")),
			MaxHeight:     atoi("max_height"),
			MaxFPS:        atoi("max_fps"),
			TenBit:        r.PostFormValue("ten_bit") == "1",
			Tonemap:       r.PostFormValue("tonemap") == "1",
			AudioBitrateK: atoi("audio_bitrate_k"),
			Stereo:        r.PostFormValue("stereo") == "1",
			AudioLang:     strings.ToLower(strings.TrimSpace(r.PostFormValue("audio_lang"))),
		},
	}
}

func (s *Server) profileError(w http.ResponseWriter, r *http.Request, err error) {
	d, derr := s.transcodingData(r.Context())
	if derr != nil {
		s.serverError(w, r, derr)
		return
	}
	d.link()
	p := s.newPage(r, "Admin - YAMP", "transcoding", d)
	p.Error = err.Error()
	s.render(w, r, http.StatusBadRequest, "admin_transcoding", p)
}

func (s *Server) handleProfileCreate(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Queue.CreateProfile(r.Context(), profileInput(r)); err != nil {
		s.profileError(w, r, err)
		return
	}
	s.redirectFlash(w, r, "/admin/transcoding", "The profile was added.")
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func (s *Server) handleProfileUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Queue.UpdateProfile(r.Context(), id, profileInput(r)); err != nil {
		s.profileError(w, r, err)
		return
	}
	s.redirectFlash(w, r, "/admin/transcoding", "The profile was saved. Renditions made with other parameters are now stale.")
}

func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Queue.DeleteProfile(r.Context(), id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectFlash(w, r, "/admin/transcoding", "The profile and its renditions were deleted.")
}

func (s *Server) handleProfileRetranscode(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	n, err := s.Queue.RetranscodeStale(r.Context(), id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectFlash(w, r, "/admin/jobs", fmt.Sprintf("%d jobs were added to the queue.", n))
}

func (s *Server) handleWorkers(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PostFormValue("workers"))
	if err != nil || n < 1 || n > 8 {
		s.redirectFlash(w, r, "/admin/transcoding", "The number of workers must be from 1 to 8.")
		return
	}
	if err := s.Settings.Set(r.Context(), settings.KeyWorkers, strconv.Itoa(n)); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectFlash(w, r, "/admin/transcoding", "Saved.")
}

func (s *Server) handleAutoEnqueue(w http.ResponseWriter, r *http.Request) {
	s.Queue.AutoEnqueue(r.Context())
	s.redirectFlash(w, r, "/admin/jobs", "The profile policies were applied to the library.")
}

// ---- Jobs ----

type jobRow struct {
	db.ListJobsRow
	Live   *transcode.Live
	Active bool
}

func (s *Server) jobRows(ctx context.Context) ([]jobRow, error) {
	jobs, err := s.Store.ListJobs(ctx, 200)
	if err != nil {
		return nil, err
	}
	rows := make([]jobRow, 0, len(jobs))
	for _, j := range jobs {
		jr := jobRow{ListJobsRow: j, Active: j.State == transcode.StateQueued || j.State == transcode.StateRunning}
		if live, ok := s.Queue.Live(j.ID); ok {
			jr.Live = &live
		}
		rows = append(rows, jr)
	}
	return rows, nil
}

func (s *Server) handleAdminJobs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.jobRows(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_jobs", s.newPage(r, "Admin - YAMP", "jobs", rows))
}

func (s *Server) handleJobsTable(w http.ResponseWriter, r *http.Request) {
	rows, err := s.jobRows(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.renderTemplate(w, r, http.StatusOK, "admin_jobs", "jobs-table", rows)
}

func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	if err := s.Queue.Cancel(r.Context(), id); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectFlash(w, r, backTo(r, "/admin/jobs"), "The job was canceled.")
}

func (s *Server) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	j, err := s.Store.GetJob(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		s.notFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if _, err := s.Queue.Enqueue(r.Context(), j.MediaID, j.ProfileID, j.Priority); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.redirectFlash(w, r, backTo(r, "/admin/jobs"), "The job is in the queue again.")
}

// backTo returns the local path in the "back" form field, or def.
func backTo(r *http.Request, def string) string {
	if b := r.PostFormValue("back"); localPath(b) {
		return b
	}
	return def
}

// localPath reports whether p is a path on this site. It rejects "//host" and
// "/\host", which browsers treat as other origins.
func localPath(p string) bool {
	return strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.ContainsAny(p, "\\\r\n\t")
}

// ---- Statistics ----

type statsData struct {
	Library      db.LibraryTotalsRow
	Plays        db.PlayTotalsRow
	TopViewed    []db.TopViewedRow
	TopRated     []db.TopRatedRow
	Renditions   int64
	CacheFree    int64
	CacheFreeOK  bool
	JobsByState  []db.CountJobsByStateRow
	Classes      []db.CountMediaByPlayabilityRow
	UserDiscreet bool
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var (
		d   statsData
		err error
	)
	steps := []func() error{
		func() error { d.Library, err = s.Store.LibraryTotals(ctx); return err },
		func() error { d.Plays, err = s.Store.PlayTotals(ctx); return err },
		func() error { d.TopViewed, err = s.Store.TopViewed(ctx, 10); return err },
		func() error { d.TopRated, err = s.Store.TopRated(ctx, 10); return err },
		func() error { d.Renditions, err = s.Store.SumRenditionSize(ctx); return err },
		func() error { d.JobsByState, err = s.Store.CountJobsByState(ctx); return err },
		func() error { d.Classes, err = s.Store.CountMediaByPlayability(ctx); return err },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			s.serverError(w, r, err)
			return
		}
	}
	d.CacheFree, d.CacheFreeOK = transcode.FreeBytes(s.Config.CacheDir)
	d.UserDiscreet = auth.UserFrom(ctx).Discreet
	s.render(w, r, http.StatusOK, "admin_stats", s.newPage(r, "Admin - YAMP", "stats", d))
}

// ---- Users ----

type usersData struct {
	Users []db.User
	Me    int64
}

func (s *Server) usersPage(w http.ResponseWriter, r *http.Request, status int, errMsg string) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	p := s.newPage(r, "Admin - YAMP", "users", usersData{Users: users, Me: auth.UserFrom(r.Context()).ID})
	p.Error = errMsg
	s.render(w, r, status, "admin_users", p)
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	s.usersPage(w, r, http.StatusOK, "")
}

func (s *Server) handleUserCreate(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.CreateUser(r.Context(), r.PostFormValue("username"), r.PostFormValue("password")); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "UNIQUE") {
			msg = "A user with this name already exists."
		}
		s.usersPage(w, r, http.StatusBadRequest, msg)
		return
	}
	s.redirectFlash(w, r, "/admin/users", "The user was added.")
}

func (s *Server) handleUserPassword(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	keep := ""
	if id == auth.UserFrom(r.Context()).ID {
		if c, err := r.Cookie(auth.CookieName); err == nil {
			keep = c.Value
		}
	}
	if err := s.Auth.SetPassword(r.Context(), id, r.PostFormValue("password"), keep); err != nil {
		s.usersPage(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s.redirectFlash(w, r, "/admin/users", "The password was changed. Other sessions of this user ended.")
}

func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.notFound(w, r)
		return
	}
	n, err := s.Store.CountUsers(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if n <= 1 {
		s.usersPage(w, r, http.StatusBadRequest, "You cannot delete the last user.")
		return
	}
	if err := s.Store.DeleteUser(r.Context(), id); err != nil {
		s.serverError(w, r, err)
		return
	}
	if id == auth.UserFrom(r.Context()).ID {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.redirectFlash(w, r, "/admin/users", "The user was deleted.")
}

// ---- Privacy ----

type privacyData struct {
	Salt     string
	Discreet bool
}

func (s *Server) handleAdminPrivacy(w http.ResponseWriter, r *http.Request) {
	d := privacyData{Salt: s.Settings.Salt(), Discreet: auth.UserFrom(r.Context()).Discreet}
	s.render(w, r, http.StatusOK, "admin_privacy", s.newPage(r, "Admin - YAMP", "privacy", d))
}

func (s *Server) handleSalt(w http.ResponseWriter, r *http.Request) {
	salt := strings.TrimSpace(r.PostFormValue("salt"))
	if r.PostFormValue("random") == "1" {
		salt = privacy.NewSalt()
	}
	if len(salt) < 8 || len(salt) > 256 {
		s.redirectFlash(w, r, "/admin/privacy", "The salt must have 8 to 256 characters.")
		return
	}
	if err := library.RotateSalt(r.Context(), s.Store, salt); err != nil {
		s.serverError(w, r, err)
		return
	}
	s.Settings.SetCached(settings.KeySalt, salt)
	s.Log.Info("salt changed; all public IDs changed")
	s.redirectFlash(w, r, "/admin/privacy", "The salt was changed. All video links changed.")
}

// ---- Server-sent events ----

// handleEvents streams "jobs" and "scan" events. htmx elements use them as
// triggers to reload their fragments.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("X-Accel-Buffering", "no")
	ch, cancel := s.Hub.Subscribe()
	defer cancel()
	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil {
		return
	}
	if err := rc.Flush(); err != nil {
		return
	}
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-r.Context().Done():
			return
		case name := <-ch:
			if name != events.Jobs && name != events.Scan {
				continue
			}
			_, err = fmt.Fprintf(w, "event: %s\ndata: %d\n\n", name, time.Now().UnixMilli())
		case <-ping.C:
			_, err = fmt.Fprint(w, ": ping\n\n")
		}
		if err == nil {
			err = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}
