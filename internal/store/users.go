// Package store: users.go holds the users and user_sessions tables (see
// migrations/0011_users_and_sessions.sql), the accounts and browser sessions
// behind form-based login (issue #279).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/samuelenocsson/slskdarr/internal/core"
)

// ErrUserNotFound is returned by UserByName and SessionUser when no matching,
// unexpired row exists. Mirrors the sql.ErrNoRows-wrapping convention used
// elsewhere in this package (see jobs.go, pipeline.go) but as a package
// sentinel, since callers here need to distinguish "not found" from "any
// other error" without reaching into database/sql.
var ErrUserNotFound = errors.New("user not found")

// ErrUsernameTaken is returned by CreateUser when username already exists
// (unique violation on users.username), so the HTTP handler can return 409
// without a redundant existence check.
var ErrUsernameTaken = errors.New("username already taken")

// userSelect is shared by every read below so the column order can never
// drift from the corresponding Scan call.
const userSelect = `SELECT id, username, password_hash, created_at FROM users`

// CountUsers reports how many accounts exist. Used to decide whether the
// first-run setup endpoint is still open (issue #279): the bootstrap window
// closes permanently once this is greater than zero.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts one account. Returns ErrUsernameTaken if username is
// already in use.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash) VALUES ($1, $2)`, username, passwordHash)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrUsernameTaken
		}
		return fmt.Errorf("create user: %w", err)
	}
	return nil
}

// UserByName looks up an account by username, for the login endpoint's
// password verification. Returns ErrUserNotFound if no such account exists.
func (s *Store) UserByName(ctx context.Context, username string) (core.User, error) {
	row := s.db.QueryRowContext(ctx, userSelect+` WHERE username = $1`, username)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.User{}, ErrUserNotFound
	}
	if err != nil {
		return core.User{}, fmt.Errorf("query user by name: %w", err)
	}
	return u, nil
}

// CreateSession inserts one browser session, keyed by the sha256 hash of the
// raw token handed to the client. The raw token itself is never stored.
func (s *Store) CreateSession(ctx context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO user_sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		tokenHash, userID, expiresAt)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

// SessionUser resolves a session token hash to its owning account, requiring
// expires_at to still be in the future. Returns ErrUserNotFound for an
// unknown or expired token so the authenticator cannot distinguish the two.
func (s *Store) SessionUser(ctx context.Context, tokenHash []byte) (core.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT users.id, users.username, users.password_hash, users.created_at
		   FROM user_sessions
		   JOIN users ON users.id = user_sessions.user_id
		  WHERE user_sessions.token_hash = $1 AND user_sessions.expires_at > now()`,
		tokenHash)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return core.User{}, ErrUserNotFound
	}
	if err != nil {
		return core.User{}, fmt.Errorf("query session user: %w", err)
	}
	return u, nil
}

// DeleteSession revokes one session by its token hash. Deleting an unknown
// hash is not an error (logout is idempotent).
func (s *Store) DeleteSession(ctx context.Context, tokenHash []byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE token_hash = $1`, tokenHash); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions removes every session whose expires_at has passed,
// returning the number of rows deleted. Intended to run periodically (see
// cmd/slskdarr/main.go) so user_sessions doesn't grow unbounded with expired
// rows over a daemon's lifetime.
func (s *Store) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM user_sessions WHERE expires_at <= now()`)
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: rows affected: %w", err)
	}
	return n, nil
}

// scanUser uses the package-level rowScanner (see jobs.go), satisfied by
// both *sql.Row and *sql.Rows, so it serves both single-row lookups above.
func scanUser(row rowScanner) (core.User, error) {
	var u core.User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt); err != nil {
		return core.User{}, err
	}
	return u, nil
}
