package observ

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
)

// Authenticator decides whether a request may access a private observability
// endpoint. It is deliberately small so deployments can replace token auth
// without changing the endpoint handlers.
type Authenticator interface {
	Authenticate(*http.Request) bool
}

// sessionCookieName is the browser session cookie set by POST
// /api/auth/setup and /login and cleared by /logout (issue #279).
const sessionCookieName = "slskdarr_session"

// SessionLookupFunc resolves a raw session-cookie token to whether it
// currently authenticates a user. Backed by app.Auth.SessionUser via
// cmd/slskdarr/main.go; a func type rather than an interface so this package
// still never needs to import internal/app or internal/store (see
// search.go's note on the same convention). A lookup error is treated
// identically to "not found" - Authenticate fails closed on a transient
// store error rather than granting access.
type SessionLookupFunc func(r *http.Request, token string) bool

// TokenAuthenticator authenticates a request by EITHER of two independent
// credentials (issue #279): the configured bearer/Basic token (machine
// access - curl, Prometheus, the Vite dev proxy), or a valid
// slskdarr_session cookie (browser form login). Either one is sufficient;
// neither present is a 401. The token is now optional - see
// cmd/slskdarr/main.go and internal/config - so a deployment can rely on
// session login alone.
//
// Tokens are hashed before comparison so comparisons always have equal
// length.
type TokenAuthenticator struct {
	tokenHash [sha256.Size]byte
	hasToken  bool
	session   SessionLookupFunc
}

// NewTokenAuthenticator returns a constant-time token authenticator. token
// may be empty, meaning no bearer/Basic credential is accepted and only
// session cookies authenticate. session may be nil, meaning no cookie is
// accepted and only the token authenticates; passing both nil/empty makes
// every request fail authentication.
func NewTokenAuthenticator(token string, session SessionLookupFunc) *TokenAuthenticator {
	a := &TokenAuthenticator{session: session}
	if token != "" {
		a.tokenHash = sha256.Sum256([]byte(token))
		a.hasToken = true
	}
	return a
}

// Authenticate implements Authenticator.
func (a *TokenAuthenticator) Authenticate(r *http.Request) bool {
	if a.session != nil {
		if cookie, err := r.Cookie(sessionCookieName); err == nil && cookie.Value != "" && a.session(r, cookie.Value) {
			return true
		}
	}
	if !a.hasToken {
		return false
	}
	candidate, ok := requestToken(r)
	candidateHash := sha256.Sum256([]byte(candidate))
	matches := subtle.ConstantTimeCompare(candidateHash[:], a.tokenHash[:]) == 1
	return ok && matches
}

func requestToken(r *http.Request) (string, bool) {
	if _, password, ok := r.BasicAuth(); ok {
		return password, password != ""
	}

	authorization := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(authorization, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

// isPrivatePath reports whether path requires authentication under
// ProtectPrivateEndpoints (issue #279).
//
// PRIVATE: anything under /api/ except /api/auth/, plus /metrics, /status,
// and anything under /debug/ (pprof).
// PUBLIC: everything else - notably the SPA shell and its static assets at
// "/", /healthz, /readyz, and /api/auth/* (setup/login/session must work
// before a session exists).
//
// This inverts the pre-#279 rule, which protected everything except
// /healthz and /readyz: back then the SPA was never served without a token,
// so the browser got a native Basic-auth dialog instead of the login form.
//
// Failure mode: a future endpoint registered on the mux outside these
// prefixes is SILENTLY PUBLIC. See the route-registration test in
// auth_test.go, which enumerates every pattern NewServer registers and
// asserts each one is classified as this function intends.
func isPrivatePath(path string) bool {
	if strings.HasPrefix(path, "/api/auth/") {
		return false
	}
	if strings.HasPrefix(path, "/api/") {
		return true
	}
	return path == "/metrics" || path == "/status" || strings.HasPrefix(path, "/debug/")
}

// ProtectPrivateEndpoints gates every private path (see isPrivatePath) behind
// auth. Browser POSTs and DELETEs to a private path additionally need a valid
// same-origin Origin header; bearer-authenticated non-browser clients may
// omit Origin, but cannot override a conflicting one (see sameOriginMutation).
// Public paths - including /api/auth/login and /api/auth/setup - never reach
// that check at all: SameSite=Strict already makes a cross-site login-CSRF
// inert, and the Vite dev proxy's changeOrigin:true rewrites Host but not
// Origin, so requiring a same-origin Origin there would break `make dev`.
func ProtectPrivateEndpoints(next http.Handler, auth Authenticator) http.Handler {
	if auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isPrivatePath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !auth.Authenticate(r) {
			w.Header().Add("WWW-Authenticate", `Basic realm="slskdarr"`)
			w.Header().Add("WWW-Authenticate", `Bearer realm="slskdarr"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if (r.Method == http.MethodPost || r.Method == http.MethodDelete) && !sameOriginMutation(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sameOriginMutation(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Bearer credentials are explicitly supplied by a client and cannot be
		// attached by a cross-origin HTML form. Basic credentials AND the
		// session cookie are both ambient in browsers (a cookie is attached to
		// every same-origin *and* cross-origin request automatically, exactly
		// like Basic's saved-password autofill), so both must always carry an
		// Origin to validate - only a request authenticated by Authorization:
		// Bearer is exempt here.
		scheme, _, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		return ok && strings.EqualFold(scheme, "Bearer")
	}

	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme, ok := requestScheme(r)
	if !ok {
		return false
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}

// requestScheme determines the scheme (http/https) this request actually
// arrived over: the connection's own TLS state, overridden by a single
// trusted X-Forwarded-Proto header (config.example.toml documents that the
// reverse proxy in front of slskdarr must discard any client-supplied
// X-Forwarded-Proto and set exactly one trusted value). ok is false when
// X-Forwarded-Proto is present but malformed - comma-separated (more than one
// hop disagreeing) or neither http nor https - in which case the caller
// should treat the request as untrustworthy rather than guess.
//
// Shared by sameOriginMutation's Origin check and newSessionCookie's Secure
// flag (auth.go) so the two can never disagree about what scheme a request
// arrived over.
func requestScheme(r *http.Request) (scheme string, ok bool) {
	scheme = "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		if strings.Contains(forwarded, ",") || (forwarded != "http" && forwarded != "https") {
			return "", false
		}
		scheme = forwarded
	}
	return scheme, true
}
