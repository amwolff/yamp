package web

import (
	"errors"
	"net/http"

	"github.com/amwolff/yamp/internal/auth"
)

type loginData struct {
	Setup    bool
	Username string
}

func (s *Server) handleSetupForm(w http.ResponseWriter, r *http.Request) {
	need, err := s.Auth.NeedsSetup(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !need {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", s.newPage(r, "Setup - YAMP", "", loginData{Setup: true}))
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	username, password := r.PostFormValue("username"), r.PostFormValue("password")
	p := s.newPage(r, "Setup - YAMP", "", loginData{Setup: true, Username: username})
	if password != r.PostFormValue("password2") {
		p.Error = "The passwords are not the same."
		s.render(w, r, http.StatusBadRequest, "login", p)
		return
	}
	err := s.Auth.Setup(r.Context(), w, username, password)
	switch {
	case errors.Is(err, auth.ErrSetupDone):
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	case err != nil:
		p.Error = sentences(err)
		s.render(w, r, http.StatusBadRequest, "login", p)
	default:
		s.Log.Info("first user created", "username", username)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if need, err := s.Auth.NeedsSetup(r.Context()); err == nil && need {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if u, _ := s.Auth.Authenticate(w, r); u != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", s.newPage(r, "Login - YAMP", "", loginData{}))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	username := r.PostFormValue("username")
	err := s.Auth.Login(r.Context(), w, username, r.PostFormValue("password"))
	if err == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	p := s.newPage(r, "Login - YAMP", "", loginData{Username: username})
	status := http.StatusUnauthorized
	switch {
	case errors.Is(err, auth.ErrRateLimited):
		status = http.StatusTooManyRequests
		p.Error = sentences(err)
	case errors.Is(err, auth.ErrBadCredentials):
		p.Error = "Wrong username or password."
	default:
		s.serverError(w, r, err)
		return
	}
	s.Log.Warn("failed login")
	s.render(w, r, status, "login", p)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Logout(w, r); err != nil {
		s.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
