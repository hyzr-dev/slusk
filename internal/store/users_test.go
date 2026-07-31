package store

import (
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestCountUsersAndCreateUser(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	n, err := s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 0 {
		t.Fatalf("CountUsers = %d, want 0 on a fresh database", n)
	}

	if err := s.CreateUser(ctx, "alice", "hashed-password"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	n, err = s.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 1 {
		t.Fatalf("CountUsers = %d, want 1", n)
	}
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateUser(ctx, "alice", "hash-one"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	err := s.CreateUser(ctx, "alice", "hash-two")
	if !errors.Is(err, ErrUsernameTaken) {
		t.Fatalf("CreateUser duplicate = %v, want ErrUsernameTaken", err)
	}
}

func TestUserByNameNotFound(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UserByName(context.Background(), "nobody")
	if !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("UserByName = %v, want ErrUserNotFound", err)
	}
}

func TestUserByNameRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateUser(ctx, "alice", "the-hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := s.UserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}
	if u.Username != "alice" || u.PasswordHash != "the-hash" || u.ID == 0 {
		t.Fatalf("UserByName = %+v, want populated Username/PasswordHash/ID", u)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateUser(ctx, "alice", "the-hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := s.UserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}

	hash := sha256.Sum256([]byte("a-raw-session-token"))
	expiresAt := time.Now().Add(time.Hour)
	if err := s.CreateSession(ctx, hash[:], u.ID, expiresAt); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	got, err := s.SessionUser(ctx, hash[:])
	if err != nil {
		t.Fatalf("SessionUser: %v", err)
	}
	if got.Username != "alice" {
		t.Fatalf("SessionUser = %+v, want alice", got)
	}

	if err := s.DeleteSession(ctx, hash[:]); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, err := s.SessionUser(ctx, hash[:]); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("SessionUser after delete = %v, want ErrUserNotFound", err)
	}

	// Deleting an already-unknown session is not an error (logout is
	// idempotent).
	if err := s.DeleteSession(ctx, hash[:]); err != nil {
		t.Fatalf("DeleteSession (already deleted): %v", err)
	}
}

func TestSessionUserRejectsExpiredSession(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateUser(ctx, "alice", "the-hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := s.UserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}

	hash := sha256.Sum256([]byte("an-expired-token"))
	if err := s.CreateSession(ctx, hash[:], u.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if _, err := s.SessionUser(ctx, hash[:]); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("SessionUser (expired) = %v, want ErrUserNotFound", err)
	}
}

func TestDeleteExpiredSessions(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.CreateUser(ctx, "alice", "the-hash"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := s.UserByName(ctx, "alice")
	if err != nil {
		t.Fatalf("UserByName: %v", err)
	}

	expiredHash := sha256.Sum256([]byte("expired-token"))
	liveHash := sha256.Sum256([]byte("live-token"))
	if err := s.CreateSession(ctx, expiredHash[:], u.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("CreateSession (expired): %v", err)
	}
	if err := s.CreateSession(ctx, liveHash[:], u.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession (live): %v", err)
	}

	n, err := s.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d, want 1", n)
	}

	if _, err := s.SessionUser(ctx, liveHash[:]); err != nil {
		t.Fatalf("SessionUser (live, should survive prune): %v", err)
	}
}
