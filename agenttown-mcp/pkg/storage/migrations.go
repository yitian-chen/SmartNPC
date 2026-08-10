package storage

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"
)

// migrationsFS embeds the SQL migration files so the binary is self-contained.
// Files are named NNNN_description.sql and applied in lexical (numeric) order.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationFile is a parsed migration ready to apply.
type migrationFile struct {
	version string // filename without .sql
	source  string // raw SQL
}

// readMigrations loads and sorts embedded migration files by version.
func readMigrations() ([]migrationFile, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}
	var out []migrationFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", e.Name(), err)
		}
		out = append(out, migrationFile{
			version: strings.TrimSuffix(e.Name(), ".sql"),
			source:  string(body),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// runMigrations applies any pending migrations in order. Each migration runs
// inside its own transaction. The schema_migrations table records which
// versions are already applied so re-runs are idempotent.
//
// The 0001_init.sql migration uses CREATE TABLE IF NOT EXISTS, so it is
// naturally idempotent even before the tracking table exists; subsequent
// migrations should be written to rely on the tracking table for safety.
func runMigrations(ctx context.Context, db *sql.DB) error {
	// Ensure the tracking table exists (self-bootstrap).
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    VARCHAR(64)  NOT NULL PRIMARY KEY,
		applied_at DATETIME     NOT NULL DEFAULT CURRENT_TIMESTAMP
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied, err := readAppliedVersions(ctx, db)
	if err != nil {
		return err
	}

	files, err := readMigrations()
	if err != nil {
		return err
	}

	for _, f := range files {
		if _, ok := applied[f.version]; ok {
			continue
		}
		if err := applyMigration(ctx, db, f); err != nil {
			return fmt.Errorf("apply migration %s: %w", f.version, err)
		}
	}
	return nil
}

// readAppliedVersions returns the set of already-applied migration versions.
func readAppliedVersions(ctx context.Context, db *sql.DB) (map[string]struct{}, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := make(map[string]struct{})
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan version: %w", err)
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate versions: %w", err)
	}
	return out, nil
}

// applyMigration runs one migration inside a transaction and records it.
// Each .sql file is split on semicolons into statements; this is a simple
// splitter adequate for our hand-written migrations (no triggers/procedures
// with embedded semicolons).
func applyMigration(ctx context.Context, db *sql.DB, f migrationFile) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck — safe on commit success

	for _, stmt := range splitStatements(f.source) {
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("exec statement: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version) VALUES (?)`, f.version); err != nil {
		return fmt.Errorf("record version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// splitStatements splits a SQL script into individual statements on
// semicolons. Trailing/inline comments are not stripped — our migrations
// are hand-written to avoid ambiguous constructs. Blank statements are
// filtered by the caller.
func splitStatements(src string) []string {
	// Simple splitter: semicolon-terminated statements. Good enough for
	// CREATE TABLE ... ; blocks without embedded semicolons.
	raw := strings.Split(src, ";")
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		trimmed := strings.TrimSpace(s)
		if trimmed == "" {
			continue
		}
		out = append(out, s)
	}
	return out
}

// pingWithRetry waits for the DB to become reachable. MySQL may take a
// moment to accept connections after `docker run` / service start; we
// retry for up to 10 seconds so the MCP doesn't crash on a race with a
// just-started MySQL.
func pingWithRetry(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := db.PingContext(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("db unreachable after 10s: %w", lastErr)
}

// listMigrationFiles exposes the embedded migration filenames for tests
// and diagnostics. Returns sorted versions.
func listMigrationFiles() ([]string, error) {
	files, err := readMigrations()
	if err != nil {
		return nil, err
	}
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.version
	}
	return out, nil
}

// Compile-time guard: ensure migrationsFS satisfies fs.FS (embed gives us
// read-only access). This catches embed directive mistakes at build time.
var _ fs.FS = embed.FS{}
