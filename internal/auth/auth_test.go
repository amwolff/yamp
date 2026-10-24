package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/amwolff/yamp/internal/store"
)

var fastArgon = Argon2Params{Memory: 1024, Time: 1, Threads: 1}

func TestPasswordRoundTrip(t *testing.T) {
	h := HashPassword("correct horse", fastArgon)
	if ok, err := VerifyPassword("correct horse", h); err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	if ok, _ := VerifyPassword("wrong", h); ok {
		t.Fatal("wrong password accepted")
	}
	if _, err := VerifyPassword("x", "$bcrypt$foo"); err == nil {
		t.Fatal("expected error for foreign hash")
	}
}

func newManager(t *testing.T) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewManager(st, true, fastArgon), st
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == CookieName {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestSetupLoginLogout(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)

	if need, _ := m.NeedsSetup(ctx); !need {
		t.Fatal("fresh database should need setup")
	}
	if err := m.Setup(ctx, httptest.NewRecorder(), "admin", "short"); err == nil {
		t.Fatal("short password accepted")
	}
	rec := httptest.NewRecorder()
	if err := m.Setup(ctx, rec, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	c := sessionCookie(t, rec)
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("weak cookie: %+v", c)
	}
	if err := m.Setup(ctx, httptest.NewRecorder(), "other", "password123"); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second setup: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(c)
	u, err := m.Authenticate(httptest.NewRecorder(), req)
	if err != nil || u == nil || u.Username != "admin" {
		t.Fatalf("authenticate = %+v, %v", u, err)
	}

	if err := m.Login(ctx, httptest.NewRecorder(), "admin", "nope-nope"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("bad login: %v", err)
	}
	if err := m.Login(ctx, httptest.NewRecorder(), "ADMIN", "password123"); err != nil {
		t.Fatalf("case-insensitive username login: %v", err)
	}

	if err := m.Logout(httptest.NewRecorder(), req); err != nil {
		t.Fatal(err)
	}
	if u, _ := m.Authenticate(httptest.NewRecorder(), req); u != nil {
		t.Fatal("session still valid after logout")
	}
}

func TestSessionExpiryAndExtension(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)
	now := time.Unix(1_700_000_000, 0)
	m.now = func() time.Time { return now }
	rec := httptest.NewRecorder()
	if err := m.Setup(ctx, rec, "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.AddCookie(sessionCookie(t, rec))

	// After two days the session is extended and a new cookie is sent.
	now = now.Add(48 * time.Hour)
	rec = httptest.NewRecorder()
	if u, _ := m.Authenticate(rec, req); u == nil {
		t.Fatal("session expired too early")
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Fatal("session not extended")
	}
	// Beyond the TTL without use, the session is gone.
	now = now.Add(SessionTTL + time.Minute)
	if u, _ := m.Authenticate(httptest.NewRecorder(), req); u != nil {
		t.Fatal("expired session accepted")
	}
}

func TestLoginRateLimit(t *testing.T) {
	ctx := context.Background()
	m, _ := newManager(t)
	if err := m.Setup(ctx, httptest.NewRecorder(), "admin", "password123"); err != nil {
		t.Fatal(err)
	}
	var limited bool
	for range 10 {
		if err := m.Login(ctx, httptest.NewRecorder(), "admin", "wrong-password"); errors.Is(err, ErrRateLimited) {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("rate limit never triggered")
	}
	if err := m.Login(ctx, httptest.NewRecorder(), "admin", "password123"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("correct password during lockout: %v", err)
	}
}
