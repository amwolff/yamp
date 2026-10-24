// Package auth implements password hashing, login sessions and the first-run
// setup flow.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"

	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
)

// Session and password limits. SessionTTL slides with use, but the expiry is
// written at most once per extendAfter.
const (
	CookieName        = "yamp_session"
	SessionTTL        = 30 * 24 * time.Hour
	extendAfter       = time.Hour
	MinPasswordLength = 8
)

// Errors returned by Manager.
var (
	ErrRateLimited    = errors.New("too many failed login attempts; wait a moment")
	ErrBadCredentials = errors.New("wrong username or password")
	ErrSetupDone      = errors.New("setup is already complete")
)

// User is the authenticated user of a request.
type User struct {
	ID       int64
	Username string
	Discreet bool
}

type ctxKey struct{}

// WithUser returns a context that carries u.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFrom returns the user stored in ctx, or nil.
func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// Manager handles logins and sessions.
type Manager struct {
	store   *store.Store
	secure  bool
	argon   Argon2Params
	limiter *rate.Limiter
	now     func() time.Time
	// dummyHash keeps the login timing similar for unknown usernames.
	dummyHash string
}

// NewManager returns a Manager. secure sets the Secure cookie attribute.
func NewManager(st *store.Store, secure bool, argon Argon2Params) *Manager {
	return &Manager{
		store:  st,
		secure: secure,
		argon:  argon,
		// Five attempts in a burst, then one every five seconds.
		limiter:   rate.NewLimiter(rate.Every(5*time.Second), 5),
		now:       time.Now,
		dummyHash: HashPassword("dummy password", argon),
	}
}

// NeedsSetup reports whether no user exists yet.
func (m *Manager) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := m.store.CountUsers(ctx)
	return n == 0, err
}

// ValidateCredentials checks the format of a new username and password.
func ValidateCredentials(username, password string) error {
	username = strings.TrimSpace(username)
	if username == "" || utf8.RuneCountInString(username) > 64 {
		return errors.New("the username must have 1 to 64 characters")
	}
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return errors.New("the password must have at least 8 characters")
	}
	return nil
}

// Setup creates the first user and logs it in, or returns ErrSetupDone.
func (m *Manager) Setup(ctx context.Context, w http.ResponseWriter, username, password string) error {
	if err := ValidateCredentials(username, password); err != nil {
		return err
	}
	hash := HashPassword(password, m.argon)
	var id int64
	err := m.store.Tx(ctx, func(q *db.Queries) error {
		n, err := q.CountUsers(ctx)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrSetupDone
		}
		id, err = q.CreateUser(ctx, db.CreateUserParams{
			Username: strings.TrimSpace(username), PwHash: hash, CreatedAt: m.now().Unix(),
		})
		return err
	})
	if err != nil {
		return err
	}
	return m.startSession(ctx, w, id)
}

// CreateUser adds a user.
func (m *Manager) CreateUser(ctx context.Context, username, password string) error {
	if err := ValidateCredentials(username, password); err != nil {
		return err
	}
	_, err := m.store.CreateUser(ctx, db.CreateUserParams{
		Username: strings.TrimSpace(username), PwHash: HashPassword(password, m.argon), CreatedAt: m.now().Unix(),
	})
	return err
}

// SetPassword changes a password and ends the user's sessions except keepToken.
func (m *Manager) SetPassword(ctx context.Context, userID int64, password, keepToken string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return errors.New("the password must have at least 8 characters")
	}
	return m.store.Tx(ctx, func(q *db.Queries) error {
		if err := q.UpdatePassword(ctx, db.UpdatePasswordParams{PwHash: HashPassword(password, m.argon), ID: userID}); err != nil {
			return err
		}
		return q.DeleteOtherSessions(ctx, db.DeleteOtherSessionsParams{UserID: userID, TokenHash: hashToken(keepToken)})
	})
}

// Login verifies the credentials and starts a session.
func (m *Manager) Login(ctx context.Context, w http.ResponseWriter, username, password string) error {
	if m.limiter.Tokens() < 1 {
		return ErrRateLimited
	}
	u, err := m.store.GetUserByUsername(ctx, strings.TrimSpace(username))
	hash := u.PwHash
	switch {
	case errors.Is(err, sql.ErrNoRows):
		hash = m.dummyHash
	case err != nil:
		return err
	}
	ok, verr := VerifyPassword(password, hash)
	if err != nil || verr != nil || !ok {
		m.limiter.Allow()
		return ErrBadCredentials
	}
	return m.startSession(ctx, w, u.ID)
}

// Logout ends the session of the request.
func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) error {
	if c, err := r.Cookie(CookieName); err == nil {
		if err := m.store.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			return err
		}
	}
	http.SetCookie(w, m.cookie("", -1))
	return nil
}

// Authenticate returns the session's user (or nil) and extends the session.
func (m *Manager) Authenticate(w http.ResponseWriter, r *http.Request) (*User, error) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	ctx := r.Context()
	th := hashToken(c.Value)
	s, err := m.store.GetSession(ctx, th)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	now := m.now()
	if s.ExpiresAt < now.Unix() {
		_ = m.store.DeleteSession(ctx, th)
		return nil, nil
	}
	if want := now.Add(SessionTTL).Unix(); want-s.ExpiresAt > int64(extendAfter/time.Second) {
		if err := m.store.ExtendSession(ctx, db.ExtendSessionParams{ExpiresAt: want, TokenHash: th}); err != nil {
			return nil, err
		}
		http.SetCookie(w, m.cookie(c.Value, int(SessionTTL/time.Second)))
	}
	return &User{ID: s.ID, Username: s.Username, Discreet: s.Discreet != 0}, nil
}

// CleanupExpired deletes expired sessions.
func (m *Manager) CleanupExpired(ctx context.Context) error {
	return m.store.DeleteExpiredSessions(ctx, m.now().Unix())
}

func (m *Manager) startSession(ctx context.Context, w http.ResponseWriter, userID int64) error {
	token := base64.RawURLEncoding.EncodeToString(randBytes(32))
	now := m.now()
	if err := m.store.CreateSession(ctx, db.CreateSessionParams{
		TokenHash: hashToken(token), UserID: userID,
		CreatedAt: now.Unix(), ExpiresAt: now.Add(SessionTTL).Unix(),
	}); err != nil {
		return err
	}
	http.SetCookie(w, m.cookie(token, int(SessionTTL/time.Second)))
	return nil
}

func (m *Manager) cookie(value string, maxAge int) *http.Cookie {
	//nolint:gosec // G124: Secure is on by default; -cookie-secure=false is for plain-HTTP development.
	return &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read never returns an error since Go 1.24.
	_, _ = rand.Read(b)
	return b
}
