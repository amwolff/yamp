// Command yamp is Yet Another Media Player, a small video server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/amwolff/yamp/internal/auth"
	"github.com/amwolff/yamp/internal/config"
	"github.com/amwolff/yamp/internal/events"
	"github.com/amwolff/yamp/internal/library"
	"github.com/amwolff/yamp/internal/probe"
	"github.com/amwolff/yamp/internal/settings"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/transcode"
	"github.com/amwolff/yamp/internal/web"
)

func main() {
	if err := runMain(); err != nil {
		fmt.Fprintln(os.Stderr, "yamp:", err)
		os.Exit(1)
	}
}

func runMain() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Getenv, os.Stderr)
}

func run(ctx context.Context, args []string, getenv func(string) string, stderr io.Writer) error {
	cfg, err := config.Parse(args, getenv, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	var level slog.Level
	if err = level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("-log-level: %w", err)
	}
	logHandler := slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})
	log := slog.New(logHandler)

	for _, root := range cfg.Libraries {
		info, statErr := os.Stat(root)
		if statErr != nil {
			return fmt.Errorf("library root: %w", statErr)
		}
		if !info.IsDir() {
			return fmt.Errorf("library root %s is not a directory", root)
		}
	}
	for _, bin := range []*string{&cfg.FFmpeg, &cfg.FFprobe} {
		p, lookErr := exec.LookPath(*bin)
		if lookErr != nil {
			return fmt.Errorf("%s not found: %w", *bin, lookErr)
		}
		*bin = p
	}
	for _, dir := range []string{cfg.DataDir, cfg.CacheDir} {
		if err = os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}

	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "yamp.db"), cfg.Fsync)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	sets, err := settings.Load(ctx, st)
	if err != nil {
		return err
	}
	if err = transcode.SeedPresets(ctx, st); err != nil {
		return err
	}

	caps := transcode.Detect(ctx, transcode.ExecRunner, cfg.FFmpeg, cfg.VAAPIDevice)
	log.Info("encoders detected", "available", caps.Available(), "tonemap", caps.CanTonemap())

	hub := events.New()
	prober := &probe.Prober{FFprobe: cfg.FFprobe}
	queue := transcode.NewQueue(transcode.QueueConfig{
		Store: st, Caps: caps, Prober: prober, FFmpeg: cfg.FFmpeg, CacheDir: cfg.CacheDir,
		Fsync: cfg.Fsync, Hub: hub, Log: log.With("component", "transcode"), Workers: sets.Workers,
	})
	if err = queue.Recover(ctx); err != nil {
		return fmt.Errorf("recover queue: %w", err)
	}
	scanner := library.New(library.Config{
		Roots: cfg.Libraries, CacheDir: cfg.CacheDir, FFmpeg: cfg.FFmpeg, Fsync: cfg.Fsync,
		Prober: prober, Store: st, Hub: hub, Log: log.With("component", "library"),
		Tonemap: caps.CanTonemap(), Salt: sets.Salt, AfterScan: queue.AutoEnqueue,
	})
	authMgr := auth.NewManager(st, cfg.CookieSecure, auth.DefaultArgon2)
	srv, err := web.New(web.Deps{
		Config: cfg, Store: st, Auth: authMgr, Settings: sets, Scanner: scanner,
		Queue: queue, Caps: caps, Hub: hub, Log: log.With("component", "web"),
	})
	if err != nil {
		return err
	}

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	if host, _, _ := net.SplitHostPort(cfg.Listen); !isLoopback(host) {
		log.Warn("YAMP listens on a non-loopback address; put it behind a TLS reverse proxy", "addr", cfg.Listen)
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Request contexts end with ctx, so server-sent event streams stop on
		// shutdown. There is no write timeout: videos stream for hours.
		BaseContext: func(net.Listener) context.Context { return ctx },
		ErrorLog:    slog.NewLogLogger(logHandler, slog.LevelWarn),
	}

	var wg sync.WaitGroup
	wg.Go(func() { queue.Run(ctx) })
	wg.Go(func() { scanner.Run(ctx, sets.ScanInterval) })
	wg.Go(func() { cleanupSessions(ctx, authMgr, log) })
	scanner.Trigger()

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()
	log.Info("YAMP is running", "addr", ln.Addr().String(), "libraries", cfg.Libraries, "data", cfg.DataDir, "cache", cfg.CacheDir)

	select {
	case <-ctx.Done():
	case err := <-errc:
		return err
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(sctx); err != nil {
		log.Warn("HTTP shutdown", "err", err)
	}
	wg.Wait()
	return nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func cleanupSessions(ctx context.Context, m *auth.Manager, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := m.CleanupExpired(ctx); err != nil && ctx.Err() == nil {
			log.Warn("session cleanup failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
