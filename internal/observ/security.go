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

// TokenAuthenticator authenticates bearer tokens for API/metrics clients and
// HTTP Basic passwords for browser access. The Basic username is ignored.
// Tokens are hashed before comparison so comparisons always have equal length.
type TokenAuthenticator struct {
	tokenHash [sha256.Size]byte
}

// NewTokenAuthenticator returns a constant-time token authenticator.
func NewTokenAuthenticator(token string) *TokenAuthenticator {
	return &TokenAuthenticator{tokenHash: sha256.Sum256([]byte(token))}
}

// Authenticate implements Authenticator.
func (a *TokenAuthenticator) Authenticate(r *http.Request) bool {
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

// ProtectPrivateEndpoints leaves only the /healthz and /readyz probes public.
// Every other UI, API, and metrics request must pass auth. Browser POSTs
// additionally need a valid same-origin Origin header; bearer-authenticated
// non-browser clients may omit Origin, but cannot override a conflicting one.
func ProtectPrivateEndpoints(next http.Handler, auth Authenticator) http.Handler {
	if auth == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		if !auth.Authenticate(r) {
			w.Header().Add("WWW-Authenticate", `Basic realm="slskdarr"`)
			w.Header().Add("WWW-Authenticate", `Bearer realm="slskdarr"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost && !sameOriginMutation(r) {
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
		// attached by a cross-origin HTML form. Basic credentials are ambient in
		// browsers, so Basic POSTs must always carry an Origin to validate.
		scheme, _, ok := strings.Cut(r.Header.Get("Authorization"), " ")
		return ok && strings.EqualFold(scheme, "Bearer")
	}

	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		if strings.Contains(forwarded, ",") || (forwarded != "http" && forwarded != "https") {
			return false
		}
		scheme = forwarded
	}
	return strings.EqualFold(u.Scheme, scheme) && strings.EqualFold(u.Host, r.Host)
}
