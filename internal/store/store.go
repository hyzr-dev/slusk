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

// applySchema splits the schema SQL into statements and executes each one.
// Every statement is either CREATE/ALTER ... IF (NOT) EXISTS or a DO block
// guarded the same way, so the apply is idempotent.
func applySchema(db *sql.DB, schemaSQL string) error {
	for _, stmt := range splitStatements(schemaSQL) {
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

// splitStatements splits schemaSQL on ';' the same way applySchema always
// has, EXCEPT:
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
func splitStatements(schemaSQL string) []string {
	var out []string
	var cur strings.Builder
	inDollarQuote := false
	inLineComment := false
	for i := 0; i < len(schemaSQL); i++ {
		c := schemaSQL[i]
		if inLineComment {
			cur.WriteByte(c)
			if c == '\n' {
				inLineComment = false
			}
			continue
		}
		if c == '-' && i+1 < len(schemaSQL) && schemaSQL[i+1] == '-' {
			inLineComment = true
			cur.WriteString("--")
			i++
			continue
		}
		if c == '$' && i+1 < len(schemaSQL) && schemaSQL[i+1] == '$' {
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

// Close closes the underlying database handle.
func (s *Store) Close() error {
	return s.db.Close()
}
