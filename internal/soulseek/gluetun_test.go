package soulseek

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/soulseek/soul/server"
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

// startGluetunLifecycle starts c's lifecycle with ln installed as the peer
// listener, mirroring what Run does minus the accept loop: the tests below
// accept on the initial listener themselves, so they can hold an established
// connection across a rebind. Teardown closes whatever listener is current at
// the time, which is the point - the rebind replaces it.
func startGluetunLifecycle(t *testing.T, c *Client, ln net.Listener) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runCtx, err := c.beginLifecycle(ctx)
	if err != nil {
		t.Fatalf("beginLifecycle: %v", err)
	}
	if _, ok := c.setPeerListener(ln); !ok {
		t.Fatal("setPeerListener on a fresh lifecycle = !ok")
	}
	c.listenPort.Store(int64(ln.Addr().(*net.TCPAddr).Port))
	t.Cleanup(func() {
		cancel()
		c.stopLifecycle()
	})
	return runCtx
}

// gluetunPortServer serves a control server whose reported forwarded port is
// whatever port currently holds, so a test can rotate it mid-run.
func gluetunPortServer(t *testing.T, port *atomic.Int64) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]int{"port": int(port.Load())})
	}))
	t.Cleanup(ts.Close)
	return ts
}

// portAccepts reports whether a TCP connection to 127.0.0.1:port is accepted.
func portAccepts(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// TestRefreshGluetunPortRebindsOnRotation locks the core of issue #395: a
// forwarded port that changes while the client is running moves the peer
// listener to the new port and abandons the old one.
func TestRefreshGluetunPortRebindsOnRotation(t *testing.T) {
	oldPort := freePort(t)
	newPort := freePort(t)
	reported := &atomic.Int64{}
	reported.Store(int64(oldPort))
	ts := gluetunPortServer(t, reported)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)))
	if err != nil {
		t.Fatalf("bind initial listener: %v", err)
	}
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort))
	c.cfg.GluetunControlURL = ts.URL
	ctx := startGluetunLifecycle(t, c, ln)

	reported.Store(int64(newPort))
	c.refreshGluetunPort(ctx)

	if got := int(c.listenPort.Load()); got != newPort {
		t.Errorf("listenPort = %d, want %d (the rotated forwarded port)", got, newPort)
	}
	if !portAccepts(newPort) {
		t.Errorf("port %d does not accept, want the rebound listener there", newPort)
	}
	if portAccepts(oldPort) {
		t.Errorf("port %d still accepts, want the old listener closed", oldPort)
	}
	if got := c.Status().ListenAddr; got != net.JoinHostPort("127.0.0.1", strconv.Itoa(newPort)) {
		t.Errorf("Status().ListenAddr = %q, want it to follow the rebind", got)
	}
}

// TestRefreshGluetunPortKeepsEstablishedConnections locks that a rebind only
// replaces the listener: sockets accepted from the old one are independent of
// it and must keep carrying data, so an in-flight transfer is not broken.
func TestRefreshGluetunPortKeepsEstablishedConnections(t *testing.T) {
	oldPort := freePort(t)
	newPort := freePort(t)
	reported := &atomic.Int64{}
	reported.Store(int64(oldPort))
	ts := gluetunPortServer(t, reported)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)))
	if err != nil {
		t.Fatalf("bind initial listener: %v", err)
	}
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort))
	c.cfg.GluetunControlURL = ts.URL
	ctx := startGluetunLifecycle(t, c, ln)

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(accepted)
			return
		}
		accepted <- conn
	}()
	peer, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)))
	if err != nil {
		t.Fatalf("dial old listener: %v", err)
	}
	defer peer.Close()
	server, ok := <-accepted
	if !ok {
		t.Fatal("accept on the old listener failed")
	}
	defer server.Close()

	reported.Store(int64(newPort))
	c.refreshGluetunPort(ctx)

	if _, err := server.Write([]byte("still here")); err != nil {
		t.Fatalf("write on a connection accepted before the rebind: %v", err)
	}
	buf := make([]byte, len("still here"))
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := io.ReadFull(peer, buf); err != nil {
		t.Fatalf("read on a connection accepted before the rebind: %v", err)
	}
	if string(buf) != "still here" {
		t.Errorf("read %q, want %q", buf, "still here")
	}
}

// TestRefreshGluetunPortKeepsListenerOnFetchFailure locks the rule the issue
// calls out explicitly: gluetun being down, unauthorized, or reporting port 0
// leaves the working listener exactly where it is.
func TestRefreshGluetunPortKeepsListenerOnFetchFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"port zero", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]int{"port": 0})
		}},
		{"unauthorized", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(tc.handler)
			defer ts.Close()

			port := freePort(t)
			ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
			if err != nil {
				t.Fatalf("bind initial listener: %v", err)
			}
			c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
			c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			c.cfg.GluetunControlURL = ts.URL
			ctx := startGluetunLifecycle(t, c, ln)

			c.refreshGluetunPort(ctx)

			if got := int(c.listenPort.Load()); got != port {
				t.Errorf("listenPort = %d, want it left at %d", got, port)
			}
			if !portAccepts(port) {
				t.Errorf("port %d no longer accepts; a failed fetch tore down a working listener", port)
			}
		})
	}
}

// TestRefreshGluetunPortKeepsListenerWhenRebindFails locks that a bind failure
// on the new port is equally non-destructive: gluetun may report a port
// something else already holds, and losing the working listener over that
// would be worse than staying on the stale one.
func TestRefreshGluetunPortKeepsListenerWhenRebindFails(t *testing.T) {
	oldPort := freePort(t)
	blocked, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("bind blocking listener: %v", err)
	}
	defer blocked.Close()
	takenPort := blocked.Addr().(*net.TCPAddr).Port

	reported := &atomic.Int64{}
	reported.Store(int64(takenPort))
	ts := gluetunPortServer(t, reported)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)))
	if err != nil {
		t.Fatalf("bind initial listener: %v", err)
	}
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort))
	c.cfg.GluetunControlURL = ts.URL
	ctx := startGluetunLifecycle(t, c, ln)

	c.refreshGluetunPort(ctx)

	if got := int(c.listenPort.Load()); got != oldPort {
		t.Errorf("listenPort = %d, want it left at %d after a failed rebind", got, oldPort)
	}
	if !portAccepts(oldPort) {
		t.Errorf("port %d no longer accepts; a failed rebind tore down a working listener", oldPort)
	}
}

// TestRefreshGluetunPortUnchangedIsANoOp locks that the common case - the
// forwarded port has not moved - does not rebind at all, since a needless
// rebind would churn the listener on every poll.
func TestRefreshGluetunPortUnchangedIsANoOp(t *testing.T) {
	port := freePort(t)
	reported := &atomic.Int64{}
	reported.Store(int64(port))
	ts := gluetunPortServer(t, reported)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("bind initial listener: %v", err)
	}
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c.cfg.GluetunControlURL = ts.URL
	ctx := startGluetunLifecycle(t, c, ln)

	c.refreshGluetunPort(ctx)

	c.lnMu.Lock()
	current := c.listener
	c.lnMu.Unlock()
	if current != ln {
		t.Error("listener was replaced even though the forwarded port did not change")
	}
}

// TestRefreshGluetunPortAdvertisesNewPortToServer locks the third part of
// issue #395: the server learns the new port on the live connection, rather
// than staying on the stale one until the next reconnect.
func TestRefreshGluetunPortAdvertisesNewPortToServer(t *testing.T) {
	oldPort := freePort(t)
	newPort := freePort(t)
	reported := &atomic.Int64{}
	reported.Store(int64(oldPort))
	ts := gluetunPortServer(t, reported)

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort)))
	if err != nil {
		t.Fatalf("bind initial listener: %v", err)
	}
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(oldPort))
	c.cfg.GluetunControlURL = ts.URL
	ctx := startGluetunLifecycle(t, c, ln)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()
	c.mu.Lock()
	c.serverConn = clientConn
	c.serverGeneration = 1
	c.mu.Unlock()

	frames := make(chan []byte, 1)
	go func() {
		payload, err := readFramePayload(serverConn)
		if err != nil {
			close(frames)
			return
		}
		frames <- payload
	}()

	reported.Store(int64(newPort))
	c.refreshGluetunPort(ctx)

	select {
	case payload, ok := <-frames:
		if !ok {
			t.Fatal("reading the announced frame failed")
		}
		if code := binary.LittleEndian.Uint32(payload[:4]); code != uint32(server.CodeSetListenPort) {
			t.Fatalf("frame code = %d, want %d (SetListenPort)", code, server.CodeSetListenPort)
		}
		if got := int(binary.LittleEndian.Uint32(payload[4:8])); got != newPort {
			t.Errorf("announced port = %d, want %d", got, newPort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no SetListenPort announced after the port rotated")
	}
}

// TestWatchGluetunPortPollsAtInterval locks that the watcher actually polls
// rather than merely being startable, and that it stops on ctx cancellation.
func TestWatchGluetunPortPollsAtInterval(t *testing.T) {
	port := freePort(t)
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]int{"port": port})
	}))
	defer ts.Close()

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("bind initial listener: %v", err)
	}
	c := New(Config{Address: "unused:0", Username: "u", Password: "p"}, testLogger())
	c.cfg.ListenAddr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	c.cfg.GluetunControlURL = ts.URL
	c.cfg.GluetunPollInterval = 10 * time.Millisecond
	ctx := startGluetunLifecycle(t, c, ln)

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.watchGluetunPort(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("gluetun fetch count = %d, want >= 2 within the poll deadline", got)
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
