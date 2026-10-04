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

	"github.com/codemug/shhttp/internal/auth"
	"github.com/codemug/shhttp/internal/config"
	"github.com/codemug/shhttp/internal/server"
	"github.com/codemug/shhttp/internal/session"
	"github.com/codemug/shhttp/internal/store"
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
		Logger:           logger,
	}, st)
	if err != nil {
		return err
	}

	go func() {
		t := time.NewTicker(cfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				sessions.Sweep(ctx, now)
			}
		}
	}()

	// Streaming responses use this context, so cancelling it ends them during
	// shutdown.
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           server.New(authn, sessions, logger, server.Options{Version: version, AllowedOrigins: cfg.AllowedOrigins}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return baseCtx },
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	tls := cfg.TLSCert != ""
	if !tls && !isLoopback(ln.Addr()) {
		logger.Warn("listening on a non-loopback address without TLS: API keys and command output travel in clear text", "addr", ln.Addr().String())
	}
	logger.Info("shhttpd listening", "addr", ln.Addr().String(), "tls", tls, "version", version, "data_dir", cfg.DataDir)

	serveErr := make(chan error, 1)
	go func() {
		if tls {
			serveErr <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
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
	if err := sessions.Shutdown(shutdownCtx); err != nil {
		logger.Error("sessions did not stop in time", "err", err)
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
