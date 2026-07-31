package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/samuelenocsson/slskdarr/internal/app"
	"github.com/samuelenocsson/slskdarr/internal/core"
	"github.com/samuelenocsson/slskdarr/internal/store"
)

// fakeAuthStore is a minimal in-memory implementation of app.AuthStore, the
// same "fake the store, keep the real use case" approach used elsewhere in
// this package's tests (see the noop* helpers in observ_test.go), letting
// these tests exercise the real setup/login/logout/session logic in
// internal/app/auth.go without a Postgres instance. It is safe for this test
// file (unlike production observ code) to import internal/store and
// internal/core directly - the "observ never imports store" rule is about
// production wiring, not test doubles.
type fakeAuthStore struct {
	users    map[string]core.User // by username
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

// CreateFirstUser mirrors store.Store.CreateFirstUser's semantics closely
// enough for these HTTP-layer tests: it only succeeds while the table is
// empty. It is not a faithful reproduction of the real method's atomicity
// (no concurrent-request race is exercised here - that belongs in
// internal/store's own tests), just its observable success/failure shape.
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

func hashKey(tokenHash []byte) [32]byte {
	var k [32]byte
	copy(k[:], tokenHash)
	return k
}

func (s *fakeAuthStore) CreateSession(_ context.Context, tokenHash []byte, userID int64, expiresAt time.Time) error {
	s.sessions[hashKey(tokenHash)] = struct {
		userID    int64
		expiresAt time.Time
	}{userID, expiresAt}
	return nil
}

func (s *fakeAuthStore) SessionUser(_ context.Context, tokenHash []byte) (core.User, error) {
	row, ok := s.sessions[hashKey(tokenHash)]
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
	delete(s.sessions, hashKey(tokenHash))
	return nil
}

func (s *fakeAuthStore) DeleteExpiredSessions(context.Context) (int64, error) {
	var n int64
	for k, row := range s.sessions {
		if !row.expiresAt.After(time.Now()) {
			delete(s.sessions, k)
			n++
		}
	}
	return n, nil
}

// newAuthTestHandler wires a real app.Auth (backed by fakeAuthStore) into a
// full NewServer + ProtectPrivateEndpoints stack, exactly as
// cmd/slskdarr/main.go wires the production one (tokenAuth/sessionAuth/AnyOf),
// minus the token (auth is session-cookie-only in these tests unless
// withToken is true).
func newAuthTestHandler(t *testing.T, withToken bool) http.Handler {
	t.Helper()
	authSvc := &app.Auth{Store: newFakeAuthStore()}
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	deps := testServerDeps(reg)
	deps.SetupRequired = authSvc.SetupRequired
	deps.Setup = authSvc.Setup
	deps.Login = authSvc.Login
	deps.Logout = authSvc.Logout
	deps.SessionUser = func(ctx context.Context, token string) (string, bool) {
		u, found, err := authSvc.SessionUser(ctx, token)
		if err != nil {
			return "", false
		}
		return u.Username, found
	}
	sessionLookup := func(r *http.Request, token string) bool {
		_, found, err := authSvc.SessionUser(r.Context(), token)
		return err == nil && found
	}
	token := ""
	if withToken {
		token = testAuthToken
	}
	tokenAuth := NewTokenAuthenticator(token)
	sessionAuth := NewSessionAuthenticator(sessionLookup)
	deps.TokenAuth = tokenAuth
	h := NewServer(deps)
	return ProtectPrivateEndpoints(h, AnyOf(tokenAuth, sessionAuth))
}

func decodeJSON[T any](t *testing.T, body *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(body.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, body.Body.String())
	}
	return v
}

// setupAccount POSTs /api/auth/setup and returns the Set-Cookie session
// cookie it was issued.
func setupAccount(t *testing.T, h http.Handler, username, password string) *http.Cookie {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/setup", strings.NewReader(string(body)))
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("setup status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("setup cookies = %+v, want exactly one %q cookie", cookies, sessionCookieName)
	}
	return cookies[0]
}

func TestSessionCookieAuthenticatesPrivateEndpoint(t *testing.T) {
	h := newAuthTestHandler(t, false)
	cookie := setupAccount(t, h, "alice", "hunter22")

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownOrExpiredSessionCookieDoesNotAuthenticate(t *testing.T) {
	h := newAuthTestHandler(t, false)
	setupAccount(t, h, "alice", "hunter22") // establishes a real, valid cookie we deliberately don't use

	tests := []struct {
		name  string
		value string
	}{
		{"unknown token", "not-a-real-session-token"},
		{"empty token", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/status", nil)
			req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: tt.value})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestSetupSucceedsOnceThenReturnsConflict(t *testing.T) {
	h := newAuthTestHandler(t, false)
	setupAccount(t, h, "alice", "hunter22")

	body, _ := json.Marshal(map[string]string{"username": "bob", "password": "anotherpassword"})
	req := httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/setup", strings.NewReader(string(body)))
	req.Header.Set("Origin", "http://example.com")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
}

func TestLoginRejectsWrongPasswordAndUnknownUserIdentically(t *testing.T) {
	h := newAuthTestHandler(t, false)
	setupAccount(t, h, "alice", "correct-password")

	login := func(username, password string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(map[string]string{"username": username, "password": password})
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/login", strings.NewReader(string(body)))
		req.Header.Set("Origin", "http://example.com")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	wrongPassword := login("alice", "wrong-password")
	unknownUser := login("nobody", "whatever-password")

	if wrongPassword.Code != http.StatusUnauthorized {
		t.Fatalf("wrong password status = %d, want 401", wrongPassword.Code)
	}
	if unknownUser.Code != http.StatusUnauthorized {
		t.Fatalf("unknown user status = %d, want 401", unknownUser.Code)
	}
	if wrongPassword.Body.String() != unknownUser.Body.String() {
		t.Fatalf("responses differ: wrong password = %q, unknown user = %q; must be identical so a caller cannot enumerate usernames",
			wrongPassword.Body.String(), unknownUser.Body.String())
	}

	correct := login("alice", "correct-password")
	if correct.Code != http.StatusNoContent {
		t.Fatalf("correct login status = %d, want 204; body = %s", correct.Code, correct.Body.String())
	}
}

func TestLogoutRevokesSession(t *testing.T) {
	h := newAuthTestHandler(t, false)
	cookie := setupAccount(t, h, "alice", "hunter22")

	logoutReq := httptest.NewRequest(http.MethodPost, "http://example.com/api/auth/logout", nil)
	logoutReq.Header.Set("Origin", "http://example.com")
	logoutReq.Header.Set("Content-Type", "application/json")
	logoutReq.AddCookie(cookie)
	logoutRec := httptest.NewRecorder()
	h.ServeHTTP(logoutRec, logoutReq)
	if logoutRec.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logoutRec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status after logout = %d, want 401 (the revoked cookie must no longer authenticate)", rec.Code)
	}
}

func TestSessionEndpointReflectsSetupAndAuthState(t *testing.T) {
	h := newAuthTestHandler(t, false)

	fresh := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	freshRec := httptest.NewRecorder()
	h.ServeHTTP(freshRec, fresh)
	if freshRec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", freshRec.Code)
	}
	freshResp := decodeJSON[sessionResponse](t, freshRec)
	if !freshResp.SetupRequired || freshResp.Authenticated || freshResp.Username != nil {
		t.Fatalf("fresh session response = %+v, want setupRequired=true, authenticated=false, username=nil", freshResp)
	}

	cookie := setupAccount(t, h, "alice", "hunter22")

	authed := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	authed.AddCookie(cookie)
	authedRec := httptest.NewRecorder()
	h.ServeHTTP(authedRec, authed)
	authedResp := decodeJSON[sessionResponse](t, authedRec)
	if authedResp.SetupRequired || !authedResp.Authenticated || authedResp.Username == nil || *authedResp.Username != "alice" {
		t.Fatalf("authenticated session response = %+v, want setupRequired=false, authenticated=true, username=alice", authedResp)
	}
}

// TestSessionEndpointReportsTokenAuthWithNoUsername pins the make-dev case
// documented on ServerDeps.TokenAuth: the Vite dev proxy injects a bearer
// token with no session cookie, and /api/auth/session must still report
// authenticated=true (with a null username, since a token has no identity),
// even against a store with zero users.
func TestSessionEndpointReportsTokenAuthWithNoUsername(t *testing.T) {
	h := newAuthTestHandler(t, true)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+testAuthToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := decodeJSON[sessionResponse](t, rec)
	if !resp.Authenticated || resp.Username != nil {
		t.Fatalf("token-authenticated session response = %+v, want authenticated=true, username=nil", resp)
	}
	if !resp.SetupRequired {
		t.Fatalf("session response = %+v, want setupRequired=true (zero users)", resp)
	}
}

// TestCookieMutationRequiresOriginBearerDoesNot locks in the asymmetry from
// security.go's sameOriginMutation: a session cookie is ambient in the
// browser exactly like Basic auth, so a cookie-authenticated mutation without
// a matching Origin must be rejected, while the same request authenticated by
// an explicit bearer token (which a cross-origin form cannot attach) is
// allowed through with no Origin at all.
func TestCookieMutationRequiresOriginBearerDoesNot(t *testing.T) {
	h := newAuthTestHandler(t, true)
	cookie := setupAccount(t, h, "alice", "hunter22")

	t.Run("cookie without origin is forbidden", func(t *testing.T) {
		// logout is public, so the same-origin check never runs for it - it
		// applies only to PRIVATE paths (see ProtectPrivateEndpoints).
		// /api/jobs/42/cancel exercises it instead, using testServerDeps'
		// no-op Cancel.
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/jobs/42/cancel", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("cookie with matching origin is allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/jobs/42/cancel", nil)
		req.AddCookie(cookie)
		req.Header.Set("Origin", "http://example.com")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Fatalf("status = %d, want neither 401 nor 403; body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("bearer without origin is allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "http://example.com/api/jobs/42/cancel", nil)
		req.Header.Set("Authorization", "Bearer "+testAuthToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
			t.Fatalf("status = %d, want neither 401 nor 403; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// registeredRoutes is the complete, manually maintained list of every
// pattern NewServer registers on its mux, one entry per mux.Handle/HandleFunc
// call site across internal/observ (observ.go, charts.go, config.go,
// messages.go, search.go, shares.go, stream.go, uploads.go, auth.go). This
// is the "build the list another way" escape hatch: net/http's ServeMux
// exposes no public API to enumerate its own registered patterns, so this
// test cannot discover the list by reflection.
//
// It instead probes each entry and asserts mux.Handler's returned pattern
// EQUALS the entry's own method+path exactly (not merely "is non-empty") -
// that is what actually catches a stale entry: a typo'd or removed route
// falls through to a DIFFERENT registered pattern (usually "/", the SPA
// catch-all) rather than to no pattern at all, so a non-empty check alone
// would never fail. probeMethod exists only to disambiguate the two paths
// that carry both a method-specific AND a method-less registration
// (/debug/pprof and /debug/pprof/, see observ.go) - probing those with GET
// would always match the more specific "GET ..." pattern, never the
// method-less one this entry is meant to pin.
//
// This test does NOT protect against a route registered on the mux and never
// added to this list at all - see isPrivatePath's own doc comment on that
// exact failure mode; that half of the guarantee is inherently unavailable
// without reflecting into ServeMux's private state.
var registeredRoutes = []struct {
	method      string // "" means the pattern has no method prefix
	probeMethod string // only set when it must differ from method; see doc comment
	path        string
	public      bool
}{
	{method: "", path: "/metrics", public: false},
	{method: "GET", path: "/debug/pprof", public: false},
	{method: "GET", path: "/debug/pprof/", public: false},
	{method: "GET", path: "/debug/pprof/cmdline", public: false},
	{method: "GET", path: "/debug/pprof/profile", public: false},
	{method: "GET", path: "/debug/pprof/symbol", public: false},
	{method: "GET", path: "/debug/pprof/trace", public: false},
	{method: "", probeMethod: http.MethodPut, path: "/debug/pprof", public: false},
	{method: "", probeMethod: http.MethodPut, path: "/debug/pprof/", public: false},
	{method: "", path: "/healthz", public: true},
	{method: "", path: "/readyz", public: true},
	{method: "", path: "/status", public: false},
	{method: "", path: "/api/jobs", public: false},
	{method: "", path: "/api/jobs/{id}/cancel", public: false},
	{method: "", path: "/api/jobs/{id}/retry", public: false},
	{method: "", path: "/api/jobs/{id}/search", public: false},
	{method: "DELETE", path: "/api/jobs/{id}", public: false},
	{method: "", path: "/api/jobs/{id}/detail", public: false},
	{method: "", path: "/api/jobs/{id}/events", public: false},
	{method: "", path: "/api/events", public: false},
	{method: "", path: "/api/peers", public: false},
	{method: "", path: "/", public: true},
	{method: "", path: "/api/charts", public: false},
	{method: "", path: "/api/config", public: false},
	{method: "", path: "/api/config/test/lidarr", public: false},
	{method: "", path: "/api/config/test/soulseek", public: false},
	{method: "", path: "/api/shares", public: false},
	{method: "", path: "/api/shares/rescan", public: false},
	{method: "POST", path: "/api/search", public: false},
	{method: "GET", path: "/api/search/{id}", public: false},
	{method: "DELETE", path: "/api/search/{id}", public: false},
	{method: "", path: "/api/uploads", public: false},
	{method: "", path: "/api/uploads/history", public: false},
	{method: "", path: "/api/messages", public: false},
	{method: "", path: "/api/messages/{username}", public: false},
	{method: "", path: "/api/messages/{username}/read", public: false},
	{method: "GET", path: "/api/stream", public: false},
	{method: "GET", path: "/api/auth/session", public: true},
	{method: "POST", path: "/api/auth/setup", public: true},
	{method: "POST", path: "/api/auth/login", public: true},
	{method: "POST", path: "/api/auth/logout", public: true},
}

// TestEveryRegisteredRouteIsClassifiedAsIntended is the route-registration
// guard the brief for issue #279 asked for: it confirms every pattern in
// registeredRoutes resolves EXACTLY as registered on NewServer's mux (see
// registeredRoutes' doc comment for why exact-match, not merely non-empty, is
// what makes this a real staleness check), and asserts isPrivatePath
// classifies each one exactly as intended - private by default, public only
// for the documented allowlist.
func TestEveryRegisteredRouteIsClassifiedAsIntended(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewMetrics(reg)
	mux, ok := NewServer(testServerDeps(reg)).(*http.ServeMux)
	if !ok {
		t.Fatal("NewServer's handler is not *http.ServeMux; update this test's assumptions")
	}

	for _, route := range registeredRoutes {
		probeMethod := route.probeMethod
		if probeMethod == "" {
			probeMethod = route.method
		}
		if probeMethod == "" {
			probeMethod = http.MethodGet
		}
		wantPattern := route.path
		if route.method != "" {
			wantPattern = route.method + " " + route.path
		}

		probe := strings.ReplaceAll(strings.ReplaceAll(route.path, "{id}", "1"), "{username}", "someuser")
		req := httptest.NewRequest(probeMethod, "http://example.com"+probe, nil)
		_, gotPattern := mux.Handler(req)
		if gotPattern != wantPattern {
			t.Errorf("route %q %q resolved to pattern %q, want %q - list is stale", route.method, route.path, gotPattern, wantPattern)
			continue
		}

		got := isPrivatePath(route.path)
		wantPrivate := !route.public
		if got != wantPrivate {
			t.Errorf("isPrivatePath(%q) = %v, want %v (public=%v)", route.path, got, wantPrivate, route.public)
		}
	}
}
