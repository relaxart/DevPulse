package database

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
)

const migrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version     TEXT PRIMARY KEY,
    applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
)`

// Migrate applies every not-yet-applied .sql file from fsys in lexical order.
// Each migration runs in its own transaction, and an advisory lock makes
// concurrent application (several app replicas starting at once) safe.
func (db *DB) Migrate(ctx context.Context, fsys fs.FS, log *slog.Logger) error {
	conn, err := db.Pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migrations: %w", redact(err))
	}
	defer conn.Release()

	// 4242 is an arbitrary but stable DevPulse-specific lock id.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(4242)"); err != nil {
		return fmt.Errorf("acquire migration lock: %w", redact(err))
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(4242)")
	}()

	if _, err := conn.Exec(ctx, migrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", redact(err))
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", redact(err))
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate schema_migrations: %w", redact(err))
	}

	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return fmt.Errorf("read migrations directory: %w", err)
	}
	var versions []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			versions = append(versions, e.Name())
		}
	}
	sort.Strings(versions)

	for _, name := range versions {
		if applied[name] {
			continue
		}
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, redact(err))
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply migration %s: %w", name, redact(err))
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("record migration %s: %w", name, redact(err))
		}
		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, redact(err))
		}
		log.Info("applied migration", "version", name)
	}
	return nil
}

// SchemaVersion returns the most recently applied migration, for /status.
func (db *DB) SchemaVersion(ctx context.Context) (string, error) {
	var v string
	err := db.Pool.QueryRow(ctx,
		"SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&v)
	if err != nil {
		return "", redact(err)
	}
	return v, nil
}
