// Package app: auth.go hosts Auth, the use case behind form-based login
// (issue #279) - account bootstrap, credential verification, and session
// lifecycle. internal/observ/auth.go is purely the HTTP edge on top of it:
// cookie framing, JSON decoding, and status-code mapping, mirroring how Jobs
// (jobs.go) separates from the job HTTP handlers in observ.go.
package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"

	"golang.org/x/crypto/bcrypt"
)

// SessionTTL is how long a browser session stays valid after login/setup - a
// fixed 90 days, deliberately not a config key (see cmd/slskdarr/main.go and
// CLAUDE.md: config adds no keys without a corresponding production rollout
// step, and there is no product reason yet to make this tunable).
const SessionTTL = 90 * 24 * time.Hour

// maxPasswordBytes matches bcrypt's own hard limit. bcrypt silently
// truncates anything past 72 bytes, which would make two different long
// passwords hash identically - rejected outright at Setup/Login instead of
// letting that footgun through.
const maxPasswordBytes = 72
const minPasswordBytes = 8
const maxUsernameBytes = 64

// ErrUsernameTaken mirrors store.ErrUsernameTaken so observ never needs to
// import internal/store (see internal/observ/search.go for the established
// reason).
var ErrUsernameTaken = errors.New("username already taken")

// ErrSetupClosed is returned by Setup once any account already exists - the
// bootstrap window closes permanently the moment the first user is created,
// with no v1 path back in short of an operator truncating the users table.
var ErrSetupClosed = errors.New("setup already completed")

// ErrInvalidCredentials is returned by Login for both an unknown username and
// a wrong password. Deliberately one error for both: a caller that could
// tell them apart could enumerate valid usernames.
var ErrInvalidCredentials = errors.New("invalid username or password")

// ErrInvalidUsername, ErrPasswordTooShort and ErrPasswordTooLong are Setup's
// input-validation errors.
var ErrInvalidUsername = errors.New("username is required and must be 64 characters or fewer")
var ErrPasswordTooShort = errors.New("password must be at least 8 characters")
var ErrPasswordTooLong = errors.New("password must be 72 bytes or fewer")

// AuthStore is the slice of *store.Store that Auth needs. Declared here
// (not in internal/store) so this package, not the store, owns the shape of
// what a use case is allowed to call - the same convention as JobStore above.
type AuthStore interface {
	CountUsers(ctx context.Context) (int, error)
	CreateFirstUser(ctx context.Context, username, passwordHash string) error
	UserByName(ctx context.Context, username string) (core.User, error)
	CreateSession(ctx context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error
	SessionUser(ctx context.Context, tokenHash []byte) (core.User, error)
	DeleteSession(ctx context.Context, tokenHash []byte) error
	DeleteExpiredSessions(ctx context.Context) (int64, error)
}

// Auth is the use case behind POST /api/auth/setup, /login, /logout and GET
// /session.
type Auth struct {
	Store AuthStore
}

// SetupRequired reports whether no account exists yet, i.e. whether Setup is
// still callable. Also backs GET /api/auth/session's setupRequired field.
func (a *Auth) SetupRequired(ctx context.Context) (bool, error) {
	n, err := a.Store.CountUsers(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

func validateUsername(username string) (string, error) {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" || len(trimmed) > maxUsernameBytes {
		return "", ErrInvalidUsername
	}
	return trimmed, nil
}

func validatePassword(password string) error {
	if len(password) < minPasswordBytes {
		return ErrPasswordTooShort
	}
	if len(password) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

// Setup creates the first account and immediately logs it in, returning a raw
// session token for the caller to set as a cookie (see observ.newSessionCookie).
// Returns ErrSetupClosed if an account already exists (checked atomically by
// the store's CreateFirstUser - see its doc comment - so two concurrent
// first-run requests cannot both succeed), or
// ErrInvalidUsername/ErrPasswordTooShort/ErrPasswordTooLong on bad input.
func (a *Auth) Setup(ctx context.Context, username, password string) (token string, expiresAt time.Time, err error) {
	username, err = validateUsername(username)
	if err != nil {
		return "", time.Time{}, err
	}
	if err := validatePassword(password); err != nil {
		return "", time.Time{}, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("hash password: %w", err)
	}
	if err := a.Store.CreateFirstUser(ctx, username, string(hash)); err != nil {
		if errors.Is(err, store.ErrSetupClosed) {
			return "", time.Time{}, ErrSetupClosed
		}
		if errors.Is(err, store.ErrUsernameTaken) {
			return "", time.Time{}, ErrUsernameTaken
		}
		return "", time.Time{}, err
	}
	user, err := a.Store.UserByName(ctx, username)
	if err != nil {
		return "", time.Time{}, err
	}
	return a.createSession(ctx, user.ID)
}

// dummyHash is compared against on an unknown username so Login's timing
// does not distinguish "no such user" from "wrong password" (see Login).
// Generated once at package init with a fixed, unused password - it is never
// itself a real credential.
var dummyHash = mustHash("slskdarr-login-timing-decoy-not-a-real-password")

func mustHash(password string) []byte {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		// bcrypt only errors on a too-long password; the fixed decoy above is
		// well under the limit, so this can only happen if bcrypt itself is
		// broken.
		panic(fmt.Sprintf("app: hash decoy password: %v", err))
	}
	return hash
}

// Login verifies credentials and, on success, creates a new session and
// returns its raw token. Returns ErrInvalidCredentials for both an unknown
// username and a wrong password.
//
// username is trimmed the same way Setup trims it before storing (see
// validateUsername) - not merely cosmetic: v1 has no password reset, so an
// operator who typed a trailing space at setup and not at login (or vice
// versa) would otherwise get an indistinguishable, permanent 401. Login does
// NOT reject an empty/oversized trimmed username with ErrInvalidUsername the
// way Setup does - it simply won't match any account, which UserByName
// already turns into the same ErrInvalidCredentials as any other unknown
// username.
func (a *Auth) Login(ctx context.Context, username, password string) (token string, expiresAt time.Time, err error) {
	username = strings.TrimSpace(username)
	user, err := a.Store.UserByName(ctx, username)
	if errors.Is(err, store.ErrUserNotFound) {
		// Run a real bcrypt comparison anyway, against the fixed decoy hash,
		// so this branch costs roughly the same wall time as the found-user
		// branch below and a timing measurement can't reveal whether the
		// username exists.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return "", time.Time{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", time.Time{}, err
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		return "", time.Time{}, ErrInvalidCredentials
	}
	return a.createSession(ctx, user.ID)
}

func (a *Auth) createSession(ctx context.Context, userID int64) (token string, expiresAt time.Time, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("generate session token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	hash := sha256.Sum256([]byte(token))
	expiresAt = time.Now().Add(SessionTTL)
	if err := a.Store.CreateSession(ctx, hash[:], userID, expiresAt); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// SessionUser resolves a raw cookie token to its account. found is false for
// an unknown or expired token, in which case user is the zero value and err
// is nil - only a genuine store failure is returned as err.
func (a *Auth) SessionUser(ctx context.Context, token string) (user core.User, found bool, err error) {
	hash := sha256.Sum256([]byte(token))
	u, err := a.Store.SessionUser(ctx, hash[:])
	if errors.Is(err, store.ErrUserNotFound) {
		return core.User{}, false, nil
	}
	if err != nil {
		return core.User{}, false, err
	}
	return u, true, nil
}

// Logout revokes a session by its raw token. Idempotent: revoking an
// already-unknown token is not an error.
func (a *Auth) Logout(ctx context.Context, token string) error {
	hash := sha256.Sum256([]byte(token))
	return a.Store.DeleteSession(ctx, hash[:])
}

// PruneExpiredSessions deletes expired session rows, returning how many were
// removed. Intended to run periodically from a background goroutine (see
// cmd/slskdarr/main.go), the same shape as the throughput recorder and share
// rescan loop already do.
func (a *Auth) PruneExpiredSessions(ctx context.Context) (int64, error) {
	return a.Store.DeleteExpiredSessions(ctx)
}
