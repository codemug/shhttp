// Command shhttpd is the shhttp server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/codemug/shhttp/v2/internal/auth"
	"github.com/codemug/shhttp/v2/internal/config"
	"github.com/codemug/shhttp/v2/internal/job"
	"github.com/codemug/shhttp/v2/internal/server"
	"github.com/codemug/shhttp/v2/internal/session"
	"github.com/codemug/shhttp/v2/internal/store"
	"github.com/codemug/shhttp/v2/internal/template"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "keygen":
			fmt.Println(auth.GenerateMasterKey())
			return
		case "version":
			fmt.Println(version)
			return
		case "openapi":
			doc, err := server.OpenAPIYAML()
			if err != nil {
				fmt.Fprintln(os.Stderr, "shhttpd:", err)
				os.Exit(1)
			}
			os.Stdout.Write(doc)
			return
		}
	}
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		config.Usage(os.Stdout)
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "shhttpd:", err)
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		slog.Error("shhttpd stopped", "err", err)
		os.Exit(1)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func run(cfg config.Config) error {
	logger := newLogger(cfg)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	st, err := store.Open(ctx, filepath.Join(cfg.DataDir, "shhttp.db"))
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer st.Close()

	masterKey, created, err := auth.LoadMasterKey(os.Getenv("SHHTTP_MASTER_KEY"), cfg.MasterKeyFile)
	if err != nil {
		return fmt.Errorf("loading master key: %w", err)
	}
	// Sessions inherit the server's environment; they must not see the key.
	os.Unsetenv("SHHTTP_MASTER_KEY")
	if created {
		logger.Warn("generated a new master key; read it from the file and keep it secret", "file", cfg.MasterKeyFile)
	}
	authn, err := auth.New(st, masterKey)
	if err != nil {
		return err
	}
	sessions, err := session.NewManager(ctx, session.Config{
		DataDir:          cfg.DataDir,
		DefaultRetention: cfg.DefaultRetention,
		KillGrace:        cfg.KillGrace,
		MaxSessions:      cfg.MaxSessions,
		MaxOutputBytes:   cfg.MaxOutputBytes,
		RunAs:            cfg.RunAs,
		Logger:           logger,
	}, st)
	if err != nil {
		return err
	}
	jobs, err := job.New(job.Config{DataDir: cfg.DataDir, DefaultRetention: cfg.DefaultRetention, Logger: logger}, st, sessions, authn.Policy)
	if err != nil {
		return err
	}
	if err := jobs.Start(ctx); err != nil {
		return fmt.Errorf("resuming jobs: %w", err)
	}

	go func() {
		t := time.NewTicker(cfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				jobs.Sweep(ctx, now)
				sessions.Sweep(ctx, now)
			}
		}
	}()

	// Streaming responses use this context, so cancelling it ends them during
	// shutdown.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Addr: cfg.Listen,
		Handler: server.New(server.Deps{Auth: authn, Sessions: sessions, Jobs: jobs, Templates: template.NewService(st)},
			logger, server.Options{Version: version, AllowedOrigins: cfg.AllowedOrigins, AuthFailureLimit: cfg.AuthFailureLimit}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	tlsConfig, err := cfg.TLSConfig()
	if err != nil {
		return err
	}
	srv.TLSConfig = tlsConfig
	tls := tlsConfig != nil
	if !tls && !isLoopback(ln.Addr()) {
		logger.Warn("listening on a non-loopback address without TLS: API keys and command output travel in clear text", "addr", ln.Addr().String())
	}
	logger.Info("shhttpd listening", "addr", ln.Addr().String(), "tls", tls, "client_certificates", cfg.ClientCA != "", "version", version, "data_dir", cfg.DataDir)

	serveErr := make(chan error, 1)
	go func() {
		if tls {
			serveErr <- srv.ServeTLS(ln, "", "")
		} else {
			serveErr <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-serveErr:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.KillGrace+5*time.Second)
	defer cancel()
	httpDone := make(chan error, 1)
	go func() { httpDone <- srv.Shutdown(shutdownCtx) }()
	// Jobs stop first so that steps killed by the shutdown are left for
	// on_restart instead of counting as failures.
	jobs.Stop()
	if err := sessions.Shutdown(shutdownCtx); err != nil {
		logger.Error("sessions did not stop in time", "err", err)
	}
	if err := jobs.Wait(shutdownCtx); err != nil {
		logger.Error("jobs did not stop in time", "err", err)
	}
	cancelBase()
	if err := <-httpDone; err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

func isLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	return ok && tcp.IP.IsLoopback()
}
