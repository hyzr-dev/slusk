// Package observ: auth.go serves form-based login (issue #279): GET
// /api/auth/session (the SPA's boot-time "am I logged in / is this a fresh
// install" check), POST /api/auth/setup (first-run account bootstrap), POST
// /api/auth/login, and POST /api/auth/logout. All four are PUBLIC (see
// isPrivatePath in security.go) - they must work before a session exists.
//
// The business logic - hashing, token generation, session TTL - lives in
// app.Auth (internal/app/auth.go); this file is purely the HTTP edge, the
// same split observ.go keeps between app.Jobs and the job handlers.
package observ

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"time"

	"github.com/hyzr-dev/slusk/internal/app"
)

// maxAuthRequestBytes bounds the raw POST /api/auth/{setup,login} request
// body (issue #279): these are public, unauthenticated endpoints that buffer
// a JSON decode, so they need the same MaxBytesReader guard
// serveSendMessage applies (messages.go) even though the payload - two short
// strings - is far smaller than a message body.
const maxAuthRequestBytes = 4096

// SetupRequiredFunc reports whether no account exists yet (backed by
// app.Auth.SetupRequired). Used by GET /api/auth/session's setupRequired
// field.
type SetupRequiredFunc func(ctx context.Context) (bool, error)

// SessionUserFunc resolves a raw session-cookie token to the username of its
// owning account. found is false for an unknown or expired token. Backed by
// app.Auth.SessionUser; the /api/auth/session handler is the only caller and
// never needs more than the username.
type SessionUserFunc func(ctx context.Context, token string) (username string, found bool)

// SetupFunc creates the first account and logs it in, returning a raw
// session token and its expiry. Backed by app.Auth.Setup. Errors are mapped
// by the handler: errors.Is(err, app.ErrSetupClosed) -> 409,
// errors.Is(err, app.ErrInvalidUsername | app.ErrPasswordTooShort |
// app.ErrPasswordTooLong) -> 400, anything else -> 500.
type SetupFunc func(ctx context.Context, username, password string) (token string, expiresAt time.Time, err error)

// LoginFunc verifies credentials and, on success, creates a session. Backed
// by app.Auth.Login. errors.Is(err, app.ErrInvalidCredentials) -> 401,
// anything else -> 500.
type LoginFunc func(ctx context.Context, username, password string) (token string, expiresAt time.Time, err error)

// LogoutFunc revokes a session by its raw token. Backed by app.Auth.Logout.
// Idempotent - revoking an already-unknown token is not an error.
type LogoutFunc func(ctx context.Context, token string) error

// authCredentials is the shared request body shape for POST
// /api/auth/setup and /login.
type authCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// sessionResponse is GET /api/auth/session's body. Username is a pointer so
// an unauthenticated or token-authenticated request serializes it as JSON
// null rather than "" - "" would read as a real, empty username.
type sessionResponse struct {
	Authenticated bool    `json:"authenticated"`
	Username      *string `json:"username"`
	SetupRequired bool    `json:"setupRequired"`
}

// registerAuth wires the four public auth endpoints onto mux. tokenAuth is
// deliberately the TOKEN-ONLY authenticator (see NewTokenAuthenticator, not
// the AnyOf-combined one cmd/slusk/main.go wraps the rest of the handler
// with) so GET /api/auth/session can tell "authenticated by the machine
// token" (username stays null) apart from "authenticated by a browser
// session" (username is populated, checked separately via sessionUser
// below) - a combined authenticator would blur that distinction depending on
// which credential it happened to try first.
func registerAuth(mux *http.ServeMux, tokenAuth Authenticator, setupRequired SetupRequiredFunc, sessionUser SessionUserFunc, setup SetupFunc, login LoginFunc, logout LogoutFunc) {
	mux.HandleFunc("GET /api/auth/session", func(w http.ResponseWriter, r *http.Request) {
		resp := sessionResponse{}
		if setupRequired != nil {
			required, err := setupRequired(r.Context())
			if err != nil {
				// Public endpoint - never echo a store error, which can carry
				// a filesystem path or the Postgres DSN (see uploads.go's
				// UploadEntry doc comment for the same house rule).
				writeConfigError(w, http.StatusInternalServerError, "failed to check session state", nil)
				return
			}
			resp.SetupRequired = required
		}
		if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" && sessionUser != nil {
			if username, found := sessionUser(r.Context(), cookie.Value); found {
				resp.Authenticated = true
				resp.Username = &username
			}
		}
		if !resp.Authenticated && tokenAuth != nil && tokenAuth.Authenticate(r) {
			// Token identity carries no user - see the Vite dev proxy note in
			// CLAUDE.md: it injects a bearer token, so `make dev` must report
			// authenticated even against a lab DB with zero users.
			resp.Authenticated = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("POST /api/auth/setup", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSONBody(w, r) {
			return
		}
		serveAuthCreate(w, r, authCreateFunc(setup), http.StatusConflict, app.ErrSetupClosed)
	})

	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if !requireJSONBody(w, r) {
			return
		}
		serveAuthCreate(w, r, authCreateFunc(login), http.StatusUnauthorized, app.ErrInvalidCredentials)
	})

	mux.HandleFunc("POST /api/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately NOT gated by requireJSONBody, unlike setup/login: logout
		// carries no body to inject credentials or a hijack-worthy side effect
		// into - a forged cross-site POST here can only force a no-op logout
		// (the worst outcome is the operator has to log back in), not create
		// an account or take over a session the way a forged setup/login body
		// could. web/src/api/client.ts's apiPost (used by useLogout) also sends
		// no Content-Type at all, so requiring one here would break it.
		if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" && logout != nil {
			// Best-effort: a store error here must not stop the client from
			// clearing its own cookie below (logout is expected to always
			// "succeed" from the browser's point of view).
			_ = logout(r.Context(), cookie.Value)
		}
		http.SetCookie(w, clearSessionCookie(r))
		w.WriteHeader(http.StatusNoContent)
	})
}

// requireJSONBody rejects a request whose Content-Type is not
// application/json with 415, writing the error response itself; callers
// should return immediately when it reports false.
//
// This is the actual CSRF defence for POST /api/auth/{setup,login} (see
// ProtectPrivateEndpoints' doc comment in security.go for why
// SameSite=Strict alone is NOT sufficient, and registerAuth's logout handler
// for why logout deliberately does NOT use this): application/json is not a
// CORS-safelisted content type, so a cross-origin HTML form - which cannot
// set an arbitrary Content-Type without triggering a preflight the browser
// then refuses to send past - cannot reach these handlers with a body they
// will decode. Parameters (e.g. "; charset=utf-8") are ignored via
// mime.ParseMediaType, matching how web/src/api/client.ts's JSON POST helper
// sets the header with no parameters.
func requireJSONBody(w http.ResponseWriter, r *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeConfigError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json", nil)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAuthRequestBytes)
	return true
}

// authCreateFunc is the shape shared by SetupFunc and LoginFunc, so
// serveAuthCreate can drive both endpoints with one implementation.
type authCreateFunc func(ctx context.Context, username, password string) (token string, expiresAt time.Time, err error)

// serveAuthCreate decodes credentials, calls create (Setup or Login), and on
// success sets the session cookie and returns 204. conflictErr identifies
// the sentinel that maps to conflictStatus (app.ErrSetupClosed -> 409 for
// setup, app.ErrInvalidCredentials -> 401 for login); every other app.Err*
// validation sentinel maps to 400; anything else maps to 500.
func serveAuthCreate(w http.ResponseWriter, r *http.Request, create authCreateFunc, conflictStatus int, conflictErr error) {
	var req authCredentials
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeConfigError(w, http.StatusBadRequest, "invalid request body", nil)
		return
	}
	if create == nil {
		http.Error(w, "not available", http.StatusServiceUnavailable)
		return
	}
	token, expiresAt, err := create(r.Context(), req.Username, req.Password)
	switch {
	case err == nil:
		http.SetCookie(w, newSessionCookie(r, token, expiresAt))
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, conflictErr):
		writeConfigError(w, conflictStatus, err.Error(), nil)
	case errors.Is(err, app.ErrUsernameTaken):
		writeConfigError(w, http.StatusConflict, err.Error(), nil)
	case errors.Is(err, app.ErrInvalidUsername), errors.Is(err, app.ErrPasswordTooShort), errors.Is(err, app.ErrPasswordTooLong):
		writeConfigError(w, http.StatusBadRequest, err.Error(), nil)
	default:
		writeConfigError(w, http.StatusInternalServerError, "authentication failed", nil)
	}
}

// sessionCookieMaxAgeFloor guards against expiresAt already being in the
// past (which would set a negative Max-Age, i.e. an immediate-delete cookie)
// - defensive only, since app.Auth always computes expiresAt as now+SessionTTL.
const sessionCookieMaxAgeFloor = 0

// requestSecureCookie decides the cookie Secure flag from requestScheme: true
// when the request provably arrived over https, and ALSO true (fail closed)
// when requestScheme can't determine the scheme at all - a malformed or
// multi-valued X-Forwarded-Proto (e.g. a two-hop proxy chain producing
// "https,https") must never silently downgrade to a cleartext cookie. See
// requestScheme's doc comment in security.go for why this resolves ok==false
// the opposite way sameOriginMutation does.
func requestSecureCookie(r *http.Request) bool {
	scheme, ok := requestScheme(r)
	return !ok || scheme == "https"
}

// newSessionCookie builds the Set-Cookie value for a freshly created session.
// See requestSecureCookie for the Secure flag's fail-closed rule.
func newSessionCookie(r *http.Request, token string, expiresAt time.Time) *http.Cookie {
	maxAge := int(time.Until(expiresAt).Seconds())
	if maxAge < sessionCookieMaxAgeFloor {
		maxAge = sessionCookieMaxAgeFloor
	}
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   requestSecureCookie(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	}
}

// clearSessionCookie builds the Set-Cookie value that deletes the session
// cookie (Max-Age=0), matching newSessionCookie's other attributes so the
// browser recognizes it as the same cookie.
func clearSessionCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   requestSecureCookie(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
}
