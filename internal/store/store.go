// Package store owns all PostgreSQL persistence and is the only package that
// runs SQL or opens transactions. All atomic state logic (write-ahead enqueue,
// deadline checks, state transitions) lives here.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Store wraps the database handle. Construct it with Open.
type Store struct {
	db *sql.DB
}

// connMaxIdleTime and connMaxLifetime bound how long a pooled connection may
// sit idle or stay open before it is proactively replaced. Without this, a
// connection can go silently dead - e.g. Docker Swarm's overlay network
// (VXLAN) expiring an idle conntrack mapping with no FIN/RST reaching either
// side - and the next query on it blocks forever: nothing at the OS or
// database/sql level notices, since the socket still looks fine. Recycling
// well before any such idle timeout turns that indefinite hang into, at worst,
// one query retried on a fresh connection.
const (
	connMaxIdleTime    = 2 * time.Minute
	connMaxLifetime    = 5 * time.Minute
	defaultOpenTimeout = 30 * time.Second
)

// Open connects to PostgreSQL with a bounded default startup context.
func Open(dsn string) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultOpenTimeout)
	defer cancel()
	return OpenContext(ctx, dsn)
}

// OpenContext connects to PostgreSQL, verifies the connection, and applies
// pending migrations within the caller's startup deadline.
func OpenContext(ctx context.Context, dsn string) (*Store, error) {
	return openWithLimitsContext(ctx, dsn, connMaxIdleTime, connMaxLifetime)
}

func openWithLimits(dsn string, maxIdleTime, maxLifetime time.Duration) (*Store, error) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultOpenTimeout)
	defer cancel()
	return openWithLimitsContext(ctx, dsn, maxIdleTime, maxLifetime)
}

func openWithLimitsContext(ctx context.Context, dsn string, maxIdleTime, maxLifetime time.Duration) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetConnMaxIdleTime(maxIdleTime)
	db.SetConnMaxLifetime(maxLifetime)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply migrations: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}

// ApplyDestructiveMigrations applies every embedded destructive migration
// (see the migrate.go package doc) not yet recorded in schema_migrations.
// Unlike Open/OpenContext, this must be invoked explicitly - it is never run
// automatically on startup.
func (s *Store) ApplyDestructiveMigrations(ctx context.Context, logger *slog.Logger) error {
	return ApplyDestructive(ctx, s.db, logger)
}
