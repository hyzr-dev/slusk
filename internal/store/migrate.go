package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Package-level migration runner.
//
// Migrations live in internal/store/migrations as SQL files named
// "%04d_description.sql" (e.g. 0001_baseline_schema.sql,
// 0002_add_foo_column.sql). The numeric prefix is the migration's version;
// versions must be unique and are applied in strictly increasing order. A
// migration file, once merged, is immutable - never edit it after it has
// shipped. If a mistake needs fixing, add a new migration that corrects it.
//
// Destructive migrations - anything that could lose data if applied
// automatically on every boot (e.g. dropping a column still read by an older
// running instance during a rolling deploy) - are named
// "%04d_description_destructive.sql". They are discovered like any other
// migration but are never applied by Migrate/OpenContext's automatic path;
// they must be applied explicitly via ApplyDestructive, e.g. by running
// `slusk -migrate-destructive`.
//
// This replaces the old approach of re-running the entire schema.sql on
// every startup with no version tracking: each migration now runs at most
// once, inside its own transaction, recorded in the schema_migrations table
// so it is never re-applied.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrationsAdvisoryLockKey is an arbitrary fixed key for a Postgres session
// advisory lock, held for the duration of a migration run so two instances
// starting up concurrently do not race to apply migrations against the same
// database.
const migrationsAdvisoryLockKey = 8829172645103

// migration is one parsed migration file: its version, filename, SQL text,
// and whether it is destructive (see package doc above).
type migration struct {
	version     int64
	name        string
	sql         string
	destructive bool
}

// loadMigrations reads every "*.sql" file directly under dir in fsys, parses
// each filename's "%04d_" version prefix, and returns the routine
// (non-destructive) and destructive migrations, each sorted by version
// ascending. It returns an error - never panics - if two files share a
// version or a filename does not match the expected pattern.
func loadMigrations(fsys fs.FS, dir string) (routine, destructive []migration, err error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, nil, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}

	seen := make(map[int64]string)
	var all []migration
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, err := parseMigrationVersion(name)
		if err != nil {
			return nil, nil, err
		}
		if existing, dup := seen[version]; dup {
			return nil, nil, fmt.Errorf("migrations: duplicate version %d: %q and %q", version, existing, name)
		}
		seen[version] = name

		contents, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return nil, nil, fmt.Errorf("read migration %q: %w", name, err)
		}
		base := strings.TrimSuffix(name, ".sql")
		all = append(all, migration{
			version:     version,
			name:        name,
			sql:         string(contents),
			destructive: strings.HasSuffix(base, "_destructive"),
		})
	}

	sort.Slice(all, func(i, j int) bool { return all[i].version < all[j].version })
	for _, m := range all {
		if m.destructive {
			destructive = append(destructive, m)
		} else {
			routine = append(routine, m)
		}
	}
	return routine, destructive, nil
}

// parseMigrationVersion extracts the leading "%04d_" numeric version prefix
// from a migration filename, e.g. "0007_add_foo_destructive.sql" -> 7. It
// requires at least one digit followed by an underscore.
func parseMigrationVersion(name string) (int64, error) {
	idx := strings.IndexByte(name, '_')
	if idx <= 0 {
		return 0, fmt.Errorf("migrations: filename %q does not match \"%%04d_description.sql\"", name)
	}
	prefix := name[:idx]
	version, err := strconv.ParseInt(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migrations: filename %q does not start with a numeric version: %w", name, err)
	}
	return version, nil
}

// Migrate brings the database up to date with every embedded routine
// (non-destructive) migration. It is idempotent: migrations already recorded
// in schema_migrations are skipped, and calling Migrate again after a
// successful run applies nothing further. It is safe to call concurrently
// from multiple instances against the same database - an advisory lock
// serializes the whole operation.
func Migrate(ctx context.Context, db *sql.DB) error {
	return migrateFromFS(ctx, db, migrationsFS, "migrations", false, nil)
}

// ApplyDestructive applies every embedded destructive migration not yet
// recorded in schema_migrations, logging each one it applies. Unlike
// Migrate, this is never called automatically from OpenContext - it must be
// invoked explicitly (see `slusk -migrate-destructive`).
func ApplyDestructive(ctx context.Context, db *sql.DB, logger *slog.Logger) error {
	return migrateFromFS(ctx, db, migrationsFS, "migrations", true, logger)
}

// migrateFromFS bootstraps the schema_migrations table, acquires the
// migration advisory lock, and applies either the routine or the destructive
// migrations discovered under dir in fsys. Taking fsys/dir as parameters
// (rather than hardcoding the embedded migrations FS) lets tests exercise the
// exact same apply/rollback/locking logic against a deliberately-broken
// fstest.MapFS.
func migrateFromFS(ctx context.Context, db *sql.DB, fsys fs.FS, dir string, destructive bool, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	routine, destructiveMigrations, err := loadMigrations(fsys, dir)
	if err != nil {
		return err
	}
	migrations := routine
	if destructive {
		migrations = destructiveMigrations
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer conn.Close()

	if err := bootstrapMigrationsTable(ctx, conn); err != nil {
		return err
	}
	if err := acquireMigrationLock(ctx, conn); err != nil {
		return err
	}
	defer releaseMigrationLock(ctx, conn)

	return applyMigrationsOnConn(ctx, conn, migrations, logger)
}

func bootstrapMigrationsTable(ctx context.Context, conn *sql.Conn) error {
	const stmt = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	if _, err := conn.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("bootstrap schema_migrations table: %w", err)
	}
	return nil
}

func acquireMigrationLock(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationsAdvisoryLockKey); err != nil {
		return fmt.Errorf("acquire migration advisory lock: %w", err)
	}
	return nil
}

// releaseMigrationLock releases the session advisory lock acquired by
// acquireMigrationLock. It deliberately ignores the caller's ctx (which may
// already be Done by the time this runs as a deferred call after a migration
// times out or the process is signalled to stop near its deadline) and uses
// a fresh, short-lived context instead - otherwise ExecContext would fail
// immediately without ever reaching Postgres, leaving the advisory lock held
// on the pooled connection until connMaxIdleTime/connMaxLifetime expires.
func releaseMigrationLock(ctx context.Context, conn *sql.Conn) {
	unlockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", migrationsAdvisoryLockKey); err != nil {
		slog.Default().Warn("release migration advisory lock", "err", err)
	}
}

// applyMigrationsOnConn applies each pending migration, in order, on conn.
// Each migration runs in its own transaction: on success its
// schema_migrations row is inserted in the same transaction before commit; on
// any error the transaction is rolled back (so no partial DDL and no
// schema_migrations row for that version persist) and the error is returned
// immediately, without attempting later migrations.
func applyMigrationsOnConn(ctx context.Context, conn *sql.Conn, migrations []migration, logger *slog.Logger) error {
	for _, m := range migrations {
		applied, err := migrationApplied(ctx, conn, m.version)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		if err := applyOneMigration(ctx, conn, m); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", m.version, m.name, err)
		}
		logger.Info("applied migration", "version", m.version, "file", m.name)
	}
	return nil
}

func migrationApplied(ctx context.Context, conn *sql.Conn, version int64) (bool, error) {
	var exists bool
	err := conn.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check migration %d applied: %w", version, err)
	}
	return exists, nil
}

func applyOneMigration(ctx context.Context, conn *sql.Conn, m migration) error {
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range splitStatements(m.sql) {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version) VALUES ($1)`, m.version,
	); err != nil {
		return fmt.Errorf("record migration version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// splitStatements splits a migration's SQL text on ';', EXCEPT:
//   - inside a $$ ... $$ dollar-quoted block (used by DO blocks for
//     conditional migrations, e.g. a guarded RENAME COLUMN): Postgres has no
//     "IF EXISTS" form for RENAME COLUMN or ADD CONSTRAINT, so those guards
//     need a DO block whose body itself contains semicolons that must NOT be
//     treated as statement boundaries.
//   - inside a "--" line comment, where a ';' is just text and must not
//     split the statement it is documenting.
//
// Tagged dollar-quotes (e.g. $tag$ ... $tag$) are NOT supported - only the
// bare $$ form used by this schema's DO blocks is recognized. If a tagged
// form is ever needed, this parser must be extended first.
func splitStatements(sql string) []string {
	var out []string
	var cur strings.Builder
	inDollarQuote := false
	inLineComment := false
	for i := 0; i < len(sql); i++ {
		c := sql[i]
		if inLineComment {
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if c == '-' && i+1 < len(sql) && sql[i+1] == '-' {
			inLineComment = true
			cur.WriteString("--")
			i++
			continue
		}
		if c == '$' && i+1 < len(sql) && sql[i+1] == '$' {
			inDollarQuote = !inDollarQuote
			cur.WriteString("$$")
			i++
			continue
		}
		if c == ';' && !inDollarQuote {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	if strings.TrimSpace(cur.String()) != "" {
		out = append(out, cur.String())
	}
	return out
}
