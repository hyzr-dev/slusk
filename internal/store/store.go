// Package store owns all PostgreSQL persistence and is the only package that
// runs SQL or opens transactions. All atomic state logic (write-ahead enqueue,
// deadline checks, state transitions) lives here.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed schema.sql
var schemaSQL string

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
	connMaxIdleTime = 2 * time.Minute
	connMaxLifetime = 5 * time.Minute
)

// Open connects to the PostgreSQL database at dsn (a postgres:// URL or
// key=value connection string), verifies the connection with a ping, and
// applies the schema idempotently.
func Open(dsn string) (*Store, error) {
	return openWithLimits(dsn, connMaxIdleTime, connMaxLifetime)
}

func openWithLimits(dsn string, maxIdleTime, maxLifetime time.Duration) (*Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetConnMaxIdleTime(maxIdleTime)
	db.SetConnMaxLifetime(maxLifetime)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	if err := applySchema(db, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// applySchema splits the schema SQL by semicolons and executes each statement.
// Every statement is CREATE ... IF NOT EXISTS, so the apply is idempotent.
func applySchema(db *sql.DB, schemaSQL string) error {
	statements := strings.Split(schemaSQL, ";")
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}
