// Package config parses flags. Each -foo-bar can also be set as YAMP_FOO_BAR;
// the flag wins.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Config is the startup configuration.
type Config struct {
	Listen       string
	DataDir      string
	CacheDir     string
	Libraries    []string
	FFmpeg       string
	FFprobe      string
	VAAPIDevice  string
	CookieSecure bool
	Fsync        bool
	LogLevel     string
}

type stringList []string

func (l *stringList) String() string { return strings.Join(*l, string(os.PathListSeparator)) }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}

// Parse parses args without the program name; getenv is usually os.Getenv.
func Parse(args []string, getenv func(string) string, out io.Writer) (Config, error) {
	var (
		c    Config
		libs stringList
		fset = flag.NewFlagSet("yamp", flag.ContinueOnError)
	)
	fset.SetOutput(out)
	fset.StringVar(&c.Listen, "listen", "127.0.0.1:8080", "HTTP listen address")
	fset.StringVar(&c.DataDir, "data-dir", "./data", "directory for the SQLite database")
	fset.StringVar(&c.CacheDir, "cache-dir", "", "directory for thumbnails, subtitles and renditions (default <data-dir>/cache)")
	fset.Var(&libs, "library", "library root directory (repeatable; env uses the OS path list separator)")
	fset.StringVar(&c.FFmpeg, "ffmpeg", "ffmpeg", "path to the ffmpeg binary")
	fset.StringVar(&c.FFprobe, "ffprobe", "ffprobe", "path to the ffprobe binary")
	fset.StringVar(&c.VAAPIDevice, "vaapi-device", "/dev/dri/renderD128", "VAAPI render device")
	fset.BoolVar(&c.CookieSecure, "cookie-secure", true, "set the Secure attribute on the session cookie")
	fset.BoolVar(&c.Fsync, "fsync", false, "fsync cache files before rename and run SQLite with synchronous=FULL")
	fset.StringVar(&c.LogLevel, "log-level", "info", "log level: debug, info, warn or error")

	// Environment values become flag defaults, so explicit flags override them.
	var envErr error
	fset.VisitAll(func(f *flag.Flag) {
		v := getenv(envName(f.Name))
		if v == "" || envErr != nil {
			return
		}
		if f.Name == "library" {
			for _, p := range filepath.SplitList(v) {
				if p != "" {
					libs = append(libs, p)
				}
			}
			return
		}
		if err := f.Value.Set(v); err != nil {
			envErr = fmt.Errorf("%s: %w", envName(f.Name), err)
		}
	})
	if envErr != nil {
		return c, envErr
	}
	envLibs := len(libs)
	if err := fset.Parse(args); err != nil {
		return c, err
	}
	if fset.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %q", fset.Args())
	}
	// Libraries given as flags replace the ones from the environment.
	if len(libs) > envLibs {
		libs = libs[envLibs:]
	}
	if len(libs) == 0 {
		return c, errors.New("at least one -library is required")
	}
	for _, l := range libs {
		abs, err := filepath.Abs(l)
		if err != nil {
			return c, err
		}
		c.Libraries = append(c.Libraries, abs)
	}
	var err error
	if c.DataDir, err = filepath.Abs(c.DataDir); err != nil {
		return c, err
	}
	if c.CacheDir == "" {
		c.CacheDir = filepath.Join(c.DataDir, "cache")
	}
	if c.CacheDir, err = filepath.Abs(c.CacheDir); err != nil {
		return c, err
	}
	return c, nil
}

func envName(flagName string) string {
	return "YAMP_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}
