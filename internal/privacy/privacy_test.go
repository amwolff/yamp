package privacy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPID(t *testing.T) {
	fp := []byte("fingerprint")
	a := PID("salt-1", fp)
	if len(a) != PIDLength {
		t.Fatalf("len = %d", len(a))
	}
	if a != PID("salt-1", fp) {
		t.Fatal("PID is not deterministic")
	}
	if a == PID("salt-2", fp) {
		t.Fatal("PID did not change with the salt")
	}
	if a == PID("salt-1", []byte("other")) {
		t.Fatal("PID did not change with the fingerprint")
	}
	if DirID("salt-1", "/lib", "a") == DirID("salt-1", "/lib", "b") {
		t.Fatal("DirID collision")
	}
}

func TestNewSalt(t *testing.T) {
	if a, b := NewSalt(), NewSalt(); len(a) != 64 || a == b {
		t.Fatalf("bad salts %q %q", a, b)
	}
}

func TestHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	Headers(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	for _, h := range []string{"Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Robots-Tag"} {
		if rec.Header().Get(h) == "" {
			t.Errorf("missing %s", h)
		}
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q", got)
	}
}
