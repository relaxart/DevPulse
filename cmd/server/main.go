// Command server runs the DevPulse web application and its synchronization
// worker for exactly one configured GitHub organization.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/relaxart/dev-pulse/internal/collector"
	"github.com/relaxart/dev-pulse/internal/config"
	"github.com/relaxart/dev-pulse/internal/database"
	"github.com/relaxart/dev-pulse/internal/github"
	devhttp "github.com/relaxart/dev-pulse/internal/http"
	"github.com/relaxart/dev-pulse/migrations"
	"github.com/relaxart/dev-pulse/web"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(); err != nil {
		// Configuration and startup errors never contain the token: config
		// validation reports variable names, never values.
		fmt.Fprintf(os.Stderr, "devpulse: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)
	log.Info("starting devpulse", "version", version, "config", cfg.Redacted())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := database.Connect(ctx, cfg.DatabaseURL, 10)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.WaitReady(ctx, 60*time.Second); err != nil {
		return fmt.Errorf("database is not reachable: %w", err)
	}

	// Migrations run before the HTTP server starts, so requests are never served
	// against an incompatible schema.
	if err := db.Migrate(ctx, migrationFS(), log); err != nil {
		return fmt.Errorf("schema migration failed: %w", err)
	}
	if err := db.MarkStaleRunsFailed(ctx); err != nil {
		log.Warn("could not clean up interrupted sync runs", "error", err)
	}

	ghClient := github.New(github.Options{
		Endpoint:     cfg.GitHubAPIURL,
		Token:        cfg.GitHubToken,
		Logger:       log.With("component", "github"),
		MinRemaining: cfg.MinRateLimit,
		Timeout:      cfg.RequestTimeout,
	})

	coll := collector.New(cfg, ghClient, db, log.With("component", "collector"))
	worker := collector.NewWorker(coll, cfg.SyncInterval, log.With("component", "sync-worker"))

	// The worker runs independently: a synchronization failure never stops the
	// web application from serving what is already in PostgreSQL.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx, cfg.SyncOnStartup)
	}()

	srv, err := devhttp.New(cfg, db, worker, web.FS, version, log.With("component", "http"))
	if err != nil {
		return err
	}

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		log.Info("http server listening", "addr", cfg.ListenAddr, "organization", cfg.GitHubOrg)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Error("http shutdown", "error", err)
	}

	select {
	case <-workerDone:
	case <-time.After(10 * time.Second):
		log.Warn("sync worker did not stop in time")
	}
	log.Info("devpulse stopped")
	return nil
}

func migrationFS() fs.FS { return migrations.FS }

func newLogger(level string) *slog.Logger {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l}))
}
