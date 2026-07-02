// Package store owns all SQLite persistence and is the only package that runs
// SQL or opens transactions. All atomic state logic (write-ahead enqueue,
// deadline checks, state transitions) lives here.
package store

import (
	_ "embed"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the database handle. Construct it with Open.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema idempotently. WAL mode is enabled for safer concurrent reads.
// Foreign key constraints are enforced on every connection.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if err := applySchema(db, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

// applySchema splits the schema SQL by semicolons and executes each statement,
// gracefully handling duplicate column errors from idempotent ALTER TABLE statements.
func applySchema(db *sql.DB, schemaSQL string) error {
	statements := strings.Split(schemaSQL, ";")
	for _, stmt := range statements {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.Exec(stmt); err != nil {
			// Ignore "duplicate column name" errors from idempotent ALTER TABLE statements
			if strings.Contains(err.Error(), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}
