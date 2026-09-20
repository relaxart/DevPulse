// Package database owns the PostgreSQL connection pool, the schema migrations
// and every SQL statement in DevPulse. All statements are parameterized.
package database

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB wraps the pgx connection pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens the pool and waits until PostgreSQL answers.
func Connect(ctx context.Context, databaseURL string, maxConns int32) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// The URL may contain a password, so never echo it back.
		return nil, fmt.Errorf("DATABASE_URL is not a valid PostgreSQL connection string: %w", redact(err))
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	cfg.MaxConnLifetime = time.Hour
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create postgres pool: %w", redact(err))
	}
	return &DB{Pool: pool}, nil
}

// WaitReady pings the database until it answers or the deadline passes.
func (db *DB) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		lastErr = db.Pool.Ping(pingCtx)
		cancel()
		if lastErr == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgres not ready after %s: %w", timeout, redact(lastErr))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Ping reports database health for /health and /ready.
func (db *DB) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return db.Pool.Ping(ctx)
}

// Close releases the pool.
func (db *DB) Close() {
	if db.Pool != nil {
		db.Pool.Close()
	}
}

// redact strips anything that looks like connection credentials from an error.
func redact(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s", sanitize(err.Error()))
}
