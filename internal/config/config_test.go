package config

import (
	"io"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse([]string{"-library", "/media"}, env(nil), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:8080" || !c.CookieSecure || c.Fsync {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.CacheDir != filepath.Join(c.DataDir, "cache") {
		t.Fatalf("cache dir = %q, want under %q", c.CacheDir, c.DataDir)
	}
	if len(c.Libraries) != 1 || c.Libraries[0] != "/media" {
		t.Fatalf("libraries = %q", c.Libraries)
	}
}

func TestParseEnvAndFlagPrecedence(t *testing.T) {
	e := env(map[string]string{
		"YAMP_LISTEN":  "0.0.0.0:9000",
		"YAMP_LIBRARY": "/a:/b",
		"YAMP_FSYNC":   "true",
	})
	c, err := Parse(nil, e, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "0.0.0.0:9000" || !c.Fsync || len(c.Libraries) != 2 {
		t.Fatalf("env not applied: %+v", c)
	}
	c, err = Parse([]string{"-listen", "127.0.0.1:1", "-library", "/c"}, e, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:1" {
		t.Fatalf("flag did not override env: %q", c.Listen)
	}
	if len(c.Libraries) != 1 || c.Libraries[0] != "/c" {
		t.Fatalf("flag libraries should replace env libraries: %q", c.Libraries)
	}
}

func TestParseRequiresLibrary(t *testing.T) {
	if _, err := Parse(nil, env(nil), io.Discard); err == nil {
		t.Fatal("expected error without -library")
	}
}

func TestParseBadEnv(t *testing.T) {
	if _, err := Parse([]string{"-library", "/x"}, env(map[string]string{"YAMP_FSYNC": "maybe"}), io.Discard); err == nil {
		t.Fatal("expected error for invalid bool")
	}
}
