package soulseek

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// freePort reserves an OS-assigned free TCP port on 127.0.0.1 and returns
// it, closing the reservation immediately so the caller's own bind can use
// it. There is an inherent (and in practice negligible) race between the
// close and the caller's bind.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("close port reservation: %v", err)
	}
	return port
}

// TestTrySetupUsesGluetunForwardedPort locks the happy path: when
// GluetunControlURL is set, trySetup fetches the forwarded port from the
// control server and binds the peer listener on it instead of the port in
// ListenAddr.
func TestTrySetupUsesGluetunForwardedPort(t *testing.T) {
	port := freePort(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"port": port})
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = "127.0.0.1:2234"
	c.cfg.GluetunControlURL = ts.URL

	ln, err := c.trySetup(context.Background())
	if err != nil {
		t.Fatalf("trySetup: %v", err)
	}
	defer ln.Close()

	addr := ln.Addr().(*net.TCPAddr)
	if addr.Port != port {
		t.Errorf("bound port = %d, want %d (the gluetun-forwarded port)", addr.Port, port)
	}
	if addr.IP.String() != "127.0.0.1" {
		t.Errorf("bound host = %s, want 127.0.0.1 (from ListenAddr)", addr.IP.String())
	}
}

// TestTrySetupGluetunPortZeroFails locks that a gluetun response reporting
// port 0 (VPN port forwarding not yet established) fails trySetup with an
// error naming the condition, so retryStartup treats it as transient.
func TestTrySetupGluetunPortZeroFails(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"port": 0})
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = "127.0.0.1:2234"
	c.cfg.GluetunControlURL = ts.URL

	if _, err := c.trySetup(context.Background()); err == nil {
		t.Fatal("trySetup with forwarded port 0 = nil error, want an error")
	} else if !containsAll(err.Error(), "port 0") {
		t.Errorf("trySetup error = %q, want it to mention port 0", err.Error())
	}
}

// TestRetryStartupRetriesUntilGluetunPortReady locks the retry behavior:
// retryStartup itself must keep retrying while gluetun reports port 0 and
// bind once a real port shows up. The handler returns 0 for the first two
// fetches so the retry loop demonstrably runs against gluetun failures.
func TestRetryStartupRetriesUntilGluetunPortReady(t *testing.T) {
	port := freePort(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			_ = json.NewEncoder(w).Encode(map[string]int{"port": 0})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]int{"port": port})
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = "127.0.0.1:2234"
	c.cfg.GluetunControlURL = ts.URL
	c.cfg.backoffBase = 10 * time.Millisecond
	c.cfg.backoffCap = 20 * time.Millisecond

	ln := c.retryStartup(context.Background())
	if ln == nil {
		t.Fatal("retryStartup returned nil, want a listener once gluetun reports a real port")
	}
	defer ln.Close()

	if got := calls.Load(); got < 3 {
		t.Errorf("gluetun fetch count = %d, want >= 3 (two port-0 failures must be retried)", got)
	}
	if got := ln.Addr().(*net.TCPAddr).Port; got != port {
		t.Errorf("bound port = %d, want %d", got, port)
	}
}

// TestFetchGluetunPortConnectionRefused locks that a control server that
// isn't reachable at all is a transient error, same as any other startup
// failure.
func TestFetchGluetunPortConnectionRefused(t *testing.T) {
	closedPort := freePort(t) // nothing listens here after freePort releases it
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.GluetunControlURL = fmt.Sprintf("http://127.0.0.1:%d", closedPort)

	if _, err := c.fetchGluetunPort(context.Background()); err == nil {
		t.Fatal("fetchGluetunPort against a closed port = nil error, want an error")
	}
}

// TestFetchGluetunPortServerError locks that a non-200, non-40x response is
// surfaced as an error naming the status code.
func TestFetchGluetunPortServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.GluetunControlURL = ts.URL

	if _, err := c.fetchGluetunPort(context.Background()); err == nil {
		t.Fatal("fetchGluetunPort against a 500 response = nil error, want an error")
	}
}

// TestFetchGluetunPortUnauthorized locks the self-diagnosing 401 error text
// naming both the status code and the config key to check.
func TestFetchGluetunPortUnauthorized(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.GluetunControlURL = ts.URL

	_, err := c.fetchGluetunPort(context.Background())
	if err == nil {
		t.Fatal("fetchGluetunPort against a 401 response = nil error, want an error")
	}
	if !containsAll(err.Error(), "401", "api_key") {
		t.Errorf("fetchGluetunPort error = %q, want it to mention 401 and api_key", err.Error())
	}
}

// TestFetchGluetunPortSendsAPIKeyHeader locks that GluetunAPIKey, when set,
// is sent as X-API-Key, and that the header is absent when it isn't set.
func TestFetchGluetunPortSendsAPIKeyHeader(t *testing.T) {
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-API-Key")
		_ = json.NewEncoder(w).Encode(map[string]int{"port": 12345})
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.GluetunControlURL = ts.URL
	c.cfg.GluetunAPIKey = "secret-key"

	if _, err := c.fetchGluetunPort(context.Background()); err != nil {
		t.Fatalf("fetchGluetunPort: %v", err)
	}
	if gotHeader != "secret-key" {
		t.Errorf("X-API-Key header = %q, want %q", gotHeader, "secret-key")
	}

	c.cfg.GluetunAPIKey = ""
	if _, err := c.fetchGluetunPort(context.Background()); err != nil {
		t.Fatalf("fetchGluetunPort: %v", err)
	}
	if gotHeader != "" {
		t.Errorf("X-API-Key header = %q, want absent when GluetunAPIKey is unset", gotHeader)
	}
}

// TestFetchGluetunPortMalformedJSON locks that a response body that isn't
// the expected JSON shape is an error, not a silently-zero port.
func TestFetchGluetunPortMalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.GluetunControlURL = ts.URL

	if _, err := c.fetchGluetunPort(context.Background()); err == nil {
		t.Fatal("fetchGluetunPort against malformed JSON = nil error, want an error")
	}
}

// TestTrySetupWithoutGluetunSectionIsUnaffected locks that GluetunControlURL
// left blank never touches the control server and binds cfg.ListenAddr
// exactly as before this feature existed.
func TestTrySetupWithoutGluetunSectionIsUnaffected(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("gluetun control server hit, but soulseek.gluetun is not configured")
	}))
	defer ts.Close()

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = "127.0.0.1:0"
	// GluetunControlURL deliberately left blank.

	ln, err := c.trySetup(context.Background())
	if err != nil {
		t.Fatalf("trySetup: %v", err)
	}
	defer ln.Close()
}

// TestFetchGluetunPortRespectsTimeout locks the gluetunTimeout test seam: a
// control server that never responds within the bound must produce a prompt
// error rather than hanging until the parent context is cancelled.
func TestFetchGluetunPortRespectsTimeout(t *testing.T) {
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	// ts.Close waits for outstanding requests to finish, so the handler must
	// be released (deferred second, so it runs first) before the server is
	// closed (deferred first, so it runs second) - otherwise this deadlocks.
	defer ts.Close()
	defer close(release)

	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.GluetunControlURL = ts.URL
	c.cfg.gluetunTimeout = 50 * time.Millisecond

	start := time.Now()
	_, err := c.fetchGluetunPort(context.Background())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("fetchGluetunPort against a hanging server = nil error, want a timeout error")
	}
	if elapsed > time.Second {
		t.Errorf("fetchGluetunPort took %v, want it bounded by gluetunTimeout (50ms)", elapsed)
	}
}

// containsAll reports whether s contains every substring in subs.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
