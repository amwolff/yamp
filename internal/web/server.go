// Package web serves YAMP's HTML pages, media files and small JSON/htmx
// endpoints.
package web

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/amwolff/yamp/internal/auth"
	"github.com/amwolff/yamp/internal/config"
	"github.com/amwolff/yamp/internal/events"
	"github.com/amwolff/yamp/internal/library"
	"github.com/amwolff/yamp/internal/privacy"
	"github.com/amwolff/yamp/internal/settings"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/transcode"
)

//go:embed templates
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Deps are the components that the web layer uses.
type Deps struct {
	Config   config.Config
	Store    *store.Store
	Auth     *auth.Manager
	Settings *settings.Settings
	Scanner  *library.Scanner
	Queue    *transcode.Queue
	Caps     *transcode.Caps
	Hub      *events.Hub
	Log      *slog.Logger
}

// Server is the HTTP layer.
type Server struct {
	Deps
	pages map[string]*template.Template
	// started is used for the static asset version.
	started time.Time
}

// pageFiles are the page templates; each one is parsed together with the layout
// and the partials.
var pageFiles = []string{
	"home", "watch", "login", "error",
	"admin_library", "admin_transcoding", "admin_jobs", "admin_stats", "admin_users", "admin_privacy",
}

// New parses the templates and returns a Server.
func New(d Deps) (*Server, error) {
	s := &Server{Deps: d, pages: map[string]*template.Template{}, started: time.Now()}
	for _, p := range pageFiles {
		t, err := template.New("").Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+p+".html")
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		s.pages[p] = t
	}
	return s, nil
}

// Handler returns the root handler with all middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(staticFS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler(http.FileServerFS(static))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("User-agent: *\nDisallow: /\n"))
	})

	mux.HandleFunc("GET /setup", s.handleSetupForm)
	mux.HandleFunc("POST /setup", s.handleSetup)
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)

	user := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.requireUser(h)) }
	user("GET /{$}", s.handleHome)
	user("GET /grid", s.handleGrid)
	user("GET /v/{pid}", s.handleWatch)
	user("GET /v/{pid}/status", s.handleWatchStatus)
	user("GET /media/{pid}/original", s.handleOriginal)
	user("GET /media/{pid}/r/{rid}", s.handleRendition)
	user("GET /media/{pid}/subs/{sid}", s.handleSubtitle)
	user("GET /thumb/{pid}", s.handleThumb)
	user("POST /api/playback/{pid}", s.handlePlayback)
	user("POST /api/rating/{pid}", s.handleRating)
	user("POST /settings/discreet", s.handleDiscreet)

	user("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/library", http.StatusSeeOther)
	})
	user("GET /admin/events", s.handleEvents)
	user("GET /admin/library", s.handleAdminLibrary)
	user("GET /admin/library/status", s.handleScanStatus)
	user("POST /admin/scan", s.handleScan)
	user("POST /admin/scan-interval", s.handleScanInterval)
	user("GET /admin/transcoding", s.handleAdminTranscoding)
	user("POST /admin/workers", s.handleWorkers)
	user("POST /admin/enqueue", s.handleAutoEnqueue)
	user("POST /admin/profiles", s.handleProfileCreate)
	user("POST /admin/profiles/{id}", s.handleProfileUpdate)
	user("POST /admin/profiles/{id}/delete", s.handleProfileDelete)
	user("POST /admin/profiles/{id}/retranscode", s.handleProfileRetranscode)
	user("GET /admin/jobs", s.handleAdminJobs)
	user("GET /admin/jobs/table", s.handleJobsTable)
	user("POST /admin/jobs/{id}/cancel", s.handleJobCancel)
	user("POST /admin/jobs/{id}/retry", s.handleJobRetry)
	user("POST /admin/media/{pid}/transcode", s.handleMediaTranscode)
	user("GET /admin/stats", s.handleAdminStats)
	user("GET /admin/users", s.handleAdminUsers)
	user("POST /admin/users", s.handleUserCreate)
	user("POST /admin/users/{id}/password", s.handleUserPassword)
	user("POST /admin/users/{id}/delete", s.handleUserDelete)
	user("GET /admin/privacy", s.handleAdminPrivacy)
	user("POST /admin/privacy/salt", s.handleSalt)

	var h http.Handler = mux
	h = http.NewCrossOriginProtection().Handler(h)
	h = privacy.Headers(h)
	h = s.logRequests(h)
	return h
}

// staticHandler makes embedded assets cacheable; their URLs carry the start
// time as a version.
func staticHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=604800, immutable")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach Flush and SetWriteDeadline.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logRequests logs the method, path and status. It never logs the query string,
// which can contain search terms.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/healthz" {
			return
		}
		s.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "dur", time.Since(start))
	})
}

// requireUser redirects requests without a session to /login (or /setup) and
// puts the user in the context.
func (s *Server) requireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := s.Auth.Authenticate(w, r)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if u == nil {
			if r.Method != http.MethodGet || r.Header.Get("HX-Request") != "" || !acceptsHTML(r) {
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			target := "/login"
			if need, err := s.Auth.NeedsSetup(r.Context()); err == nil && need {
				target = "/setup"
			}
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), u)))
	})
}

func acceptsHTML(r *http.Request) bool {
	a := r.Header.Get("Accept")
	return a == "" || strings.Contains(a, "text/html") || strings.Contains(a, "*/*")
}

// page is the data of every full page.
type page struct {
	// Title is the document title. It must never contain a media title.
	Title string
	User  *auth.User
	Nav   string
	Flash string
	Error string
	// Version busts the cache of static assets.
	Version string
	Data    any
}

func (s *Server) newPage(r *http.Request, title, nav string, data any) *page {
	return &page{
		Title:   title,
		User:    auth.UserFrom(r.Context()),
		Nav:     nav,
		Version: fmt.Sprint(s.started.Unix()),
		Data:    data,
	}
}

// render executes a page into a buffer, so template errors become a clean 500.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, p *page) {
	if p.Flash == "" {
		p.Flash = s.takeFlash(w, r)
	}
	s.renderTemplate(w, r, status, name, "layout", p)
}

func (s *Server) renderTemplate(w http.ResponseWriter, r *http.Request, status int, pageName, tmpl string, data any) {
	t, ok := s.pages[pageName]
	if !ok {
		s.serverError(w, r, fmt.Errorf("unknown page %q", pageName))
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, tmpl, data); err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) {
		return
	}
	s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	s.render(w, r, http.StatusNotFound, "error", s.newPage(r, "Not found - YAMP", "", "This page does not exist."))
}

const flashCookie = "yamp_flash"

func (s *Server) flashCookie(value string, maxAge int) *http.Cookie {
	//nolint:gosec // G124: Secure follows -cookie-secure, which is on by default.
	return &http.Cookie{
		Name: flashCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: s.Config.CookieSecure, SameSite: http.SameSiteStrictMode,
	}
}

// redirectFlash redirects to a local path with a one-time message.
func (s *Server) redirectFlash(w http.ResponseWriter, r *http.Request, target, msg string) {
	if !localPath(target) {
		target = "/"
	}
	if msg != "" {
		http.SetCookie(w, s.flashCookie(url.QueryEscape(msg), 60))
	}
	http.Redirect(w, r, target, http.StatusSeeOther) //nolint:gosec // G710: target is a checked local path
}

func (s *Server) takeFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil {
		return ""
	}
	http.SetCookie(w, s.flashCookie("", -1))
	msg, _ := url.QueryUnescape(c.Value)
	return msg
}
