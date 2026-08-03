package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slusk/internal/core"
	"github.com/samuelenocsson/slusk/internal/store"
)

// fakeAuthStore is a minimal in-memory AuthStore, the same "fake the store"
// approach fakeJobStore uses in jobs_test.go.
type fakeAuthStore struct {
	users    map[string]core.User
	nextID   int64
	sessions map[[32]byte]struct {
		userID    int64
		expiresAt time.Time
	}
}

func newFakeAuthStore() *fakeAuthStore {
	return &fakeAuthStore{
		users: map[string]core.User{},
		sessions: map[[32]byte]struct {
			userID    int64
			expiresAt time.Time
		}{},
	}
}

func (s *fakeAuthStore) CountUsers(context.Context) (int, error) { return len(s.users), nil }

func (s *fakeAuthStore) CreateFirstUser(_ context.Context, username, passwordHash string) error {
	if len(s.users) > 0 {
		return store.ErrSetupClosed
	}
	if _, exists := s.users[username]; exists {
		return store.ErrUsernameTaken
	}
	s.nextID++
	s.users[username] = core.User{ID: s.nextID, Username: username, PasswordHash: passwordHash}
	return nil
}

func (s *fakeAuthStore) UserByName(_ context.Context, username string) (core.User, error) {
	u, ok := s.users[username]
	if !ok {
		return core.User{}, store.ErrUserNotFound
	}
	return u, nil
}

func authFakeHashKey(tokenHash []byte) [32]byte {
	var k [32]byte
	copy(k[:], tokenHash)
	return k
}

func (s *fakeAuthStore) CreateSession(_ context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error {
	s.sessions[authFakeHashKey(tokenHash)] = struct {
		userID    int64
		expiresAt time.Time
	}{userID, expiresAt}
	return nil
}

func (s *fakeAuthStore) SessionUser(_ context.Context, tokenHash []byte) (core.User, error) {
	row, ok := s.sessions[authFakeHashKey(tokenHash)]
	if !ok || !row.expiresAt.After(time.Now()) {
		return core.User{}, store.ErrUserNotFound
	}
	for _, u := range s.users {
		if u.ID == row.userID {
			return u, nil
		}
	}
	return core.User{}, store.ErrUserNotFound
}

func (s *fakeAuthStore) DeleteSession(_ context.Context, tokenHash []byte) error {
	delete(s.sessions, authFakeHashKey(tokenHash))
	return nil
}

func (s *fakeAuthStore) DeleteExpiredSessions(context.Context) (int64, error) {
	return 0, nil
}

// TestSetupValidatesUsernameAndPassword pins the input-validation rules
// Setup enforces before ever touching the store, including the least
// obvious one: bcrypt silently truncates a password past 72 bytes, so
// ErrPasswordTooLong exists to reject that outright rather than let two
// different long passwords hash identically. None of these were asserted
// anywhere before this test.
func TestSetupValidatesUsernameAndPassword(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		wantErr  error
	}{
		{name: "empty username", username: "", password: "a-valid-password", wantErr: ErrInvalidUsername},
		{name: "whitespace-only username", username: "   ", password: "a-valid-password", wantErr: ErrInvalidUsername},
		{name: "username over 64 chars", username: strings.Repeat("a", 65), password: "a-valid-password", wantErr: ErrInvalidUsername},
		{name: "username exactly 64 chars is valid", username: strings.Repeat("a", 64), password: "a-valid-password", wantErr: nil},
		{name: "password under 8 chars", username: "alice", password: "short1", wantErr: ErrPasswordTooShort},
		{name: "password exactly 8 chars is valid", username: "alice", password: "exactly8", wantErr: nil},
		{name: "password over 72 bytes", username: "alice", password: strings.Repeat("a", 73), wantErr: ErrPasswordTooLong},
		{name: "password exactly 72 bytes is valid", username: "alice", password: strings.Repeat("a", 72), wantErr: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &Auth{Store: newFakeAuthStore()}
			_, _, err := a.Setup(context.Background(), tt.username, tt.password)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Setup(%q, <%d bytes>) err = %v, want %v", tt.username, len(tt.password), err, tt.wantErr)
			}
		})
	}
}

// TestSetupTrimsUsername pins that Setup stores a trimmed username - Login's
// own trimming (see TestLoginTrimsUsername) only matches this if both sides
// agree on what gets stored.
func TestSetupTrimsUsername(t *testing.T) {
	a := &Auth{Store: newFakeAuthStore()}
	if _, _, err := a.Setup(context.Background(), "  alice  ", "a-valid-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if _, err := a.Store.UserByName(context.Background(), "alice"); err != nil {
		t.Fatalf("UserByName(\"alice\"): %v, want the trimmed username to have been stored", err)
	}
}

// TestSetupClosesAfterFirstAccount pins ErrSetupClosed once an account
// already exists.
func TestSetupClosesAfterFirstAccount(t *testing.T) {
	a := &Auth{Store: newFakeAuthStore()}
	if _, _, err := a.Setup(context.Background(), "alice", "a-valid-password"); err != nil {
		t.Fatalf("first Setup: %v", err)
	}
	_, _, err := a.Setup(context.Background(), "bob", "another-valid-password")
	if !errors.Is(err, ErrSetupClosed) {
		t.Fatalf("second Setup err = %v, want ErrSetupClosed", err)
	}
}

// TestLoginTrimsUsername is the regression test for issue #279's fix 4: an
// operator who types a trailing/leading space at setup or at login must not
// get a permanently indistinguishable 401 - v1 has no password reset, so
// DELETE FROM users would be the only way out of that.
func TestLoginTrimsUsername(t *testing.T) {
	a := &Auth{Store: newFakeAuthStore()}
	if _, _, err := a.Setup(context.Background(), "  alice  ", "a-valid-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	tests := []struct {
		name     string
		username string
	}{
		{"untrimmed login matches trimmed setup", "  alice  "},
		{"trimmed login matches trimmed setup", "alice"},
		{"trailing space only", "alice "},
		{"leading space only", " alice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := a.Login(context.Background(), tt.username, "a-valid-password"); err != nil {
				t.Fatalf("Login(%q): %v, want success", tt.username, err)
			}
		})
	}
}

// TestLoginRejectsWrongPasswordAndUnknownUser pins ErrInvalidCredentials for
// both failure modes at the app.Auth layer (internal/observ/auth_test.go
// separately pins that the two are indistinguishable over HTTP).
func TestLoginRejectsWrongPasswordAndUnknownUser(t *testing.T) {
	a := &Auth{Store: newFakeAuthStore()}
	if _, _, err := a.Setup(context.Background(), "alice", "correct-password"); err != nil {
		t.Fatalf("Setup: %v", err)
	}

	if _, _, err := a.Login(context.Background(), "alice", "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login (wrong password) err = %v, want ErrInvalidCredentials", err)
	}
	if _, _, err := a.Login(context.Background(), "nobody", "whatever"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("Login (unknown user) err = %v, want ErrInvalidCredentials", err)
	}
}
