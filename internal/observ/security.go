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

// TokenAuthenticator authenticates the configured bearer/Basic token alone
// (machine access - curl, Prometheus, the Vite dev proxy). It knows nothing
// about session cookies - see SessionAuthenticator and AnyOf for how
// cmd/slskdarr/main.go combines the two, and ServerDeps.TokenAuth's doc
// comment for why GET /api/auth/session specifically needs the token-only
// instance rather than the combined one.
//
// Tokens are hashed before comparison so comparisons always have equal
// length.
type TokenAuthenticator struct {
	tokenHash [sha256.Size]byte
	hasToken  bool
}

// NewTokenAuthenticator returns a constant-time token authenticator. token
// may be empty (issue #279 made it optional - see internal/config), meaning
// this authenticator rejects every request; combine it with a
// SessionAuthenticator via AnyOf so a deployment can still rely on session
// login alone.
func NewTokenAuthenticator(token string) *TokenAuthenticator {
	a := &TokenAuthenticator{}
	if token != "" {
		a.tokenHash = sha256.Sum256([]byte(token))
		a.hasToken = true
	}
	return a
}

// Authenticate implements Authenticator.
func (a *TokenAuthenticator) Authenticate(r *http.Request) bool {
	if !a.hasToken {
		return false
	}
	candidate, ok := requestToken(r)
	candidateHash := sha256.Sum256([]byte(candidate))
	matches := subtle.ConstantTimeCompare(candidateHash[:], a.tokenHash[:]) == 1
	return ok && matches
}

// SessionAuthenticator authenticates a request by its slskdarr_session
// cookie alone (browser form login, issue #279). It knows nothing about the
// bearer/Basic token - see TokenAuthenticator and AnyOf.
type SessionAuthenticator struct {
	lookup SessionLookupFunc
}

// NewSessionAuthenticator returns a session-cookie authenticator. lookup may
// be nil, meaning this authenticator rejects every request (no cookie ever
// authenticates).
func NewSessionAuthenticator(lookup SessionLookupFunc) *SessionAuthenticator {
	return &SessionAuthenticator{lookup: lookup}
}

// Authenticate implements Authenticator.
func (a *SessionAuthenticator) Authenticate(r *http.Request) bool {
	if a.lookup == nil {
		return false
	}
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return a.lookup(r, cookie.Value)
}

// anyOfAuthenticator authenticates a request if ANY of its member
// Authenticators does. See AnyOf.
type anyOfAuthenticator []Authenticator

// AnyOf combines several Authenticators into one that accepts a request
// authenticated by any of them (issue #279: the bearer/Basic token and the
// session cookie are independent, equally sufficient credentials). A nil
// member is skipped rather than panicking, so a caller can pass an
// Authenticator that might itself be nil without a separate check.
func AnyOf(auths ...Authenticator) Authenticator {
	return anyOfAuthenticator(auths)
}

// Authenticate implements Authenticator.
func (a anyOfAuthenticator) Authenticate(r *http.Request) bool {
	for _, auth := range a {
		if auth != nil && auth.Authenticate(r) {
			return true
		}
	}
	return false
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
// that check at all: the Vite dev proxy's changeOrigin:true rewrites Host but
// not Origin, so requiring a same-origin Origin there would break `make dev`.
// SameSite=Strict does NOT make login/setup CSRF-safe on its own - it governs
// whether the browser SENDS a cookie, not whether it STORES a Set-Cookie from
// a cross-site response, so an attacker's page can still trigger a same-site
// POST that creates the first account or swaps a victim onto an attacker
// session. What actually defends /api/auth/login and /api/auth/setup is the
// Content-Type: application/json requirement in auth.go's requireJSONBody:
// application/json is not one of the CORS-safelisted content types, so a
// cross-origin form (which cannot set arbitrary headers without triggering a
// preflight, and application/json's preflight would fail for a truly
// cross-origin request) cannot reach these handlers with a body they accept.
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
			// Deliberately no `Basic` challenge: that header's only effect is to
			// make a browser open its native credential dialog, which is exactly
			// what the login form at / replaces (issue #279). Advertising it here
			// pops that dialog on top of our own login screen, and a stale
			// credential the browser then replays makes GET /api/auth/session
			// answer authenticated:true, silently skipping first-run setup on
			// every install that ever used the pre-#279 prompt. Basic is still
			// ACCEPTED for machine callers - curl -u sends it preemptively and
			// never needs the challenge.
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
// X-Forwarded-Proto is present but malformed - comma-separated (e.g. a
// two-hop proxy chain each appending its own value, such as "https,https")
// or neither http nor https.
//
// Shared by sameOriginMutation's Origin check and newSessionCookie's Secure
// flag (auth.go), but the two callers MUST fail closed in opposite
// directions on ok==false, because "closed" means something different for
// each: sameOriginMutation cannot prove the Origin matches, so it rejects the
// request (returns false); newSessionCookie cannot prove the request is
// plain http, so it must still mark the cookie Secure (assume https) rather
// than silently sending the session token in the clear. Treating this
// function's zero-value scheme ("") as anything in particular is a mistake -
// always check ok, and always resolve it in the safer direction for what
// you're deciding.
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
