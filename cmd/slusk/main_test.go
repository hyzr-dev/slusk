package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hyzr-dev/slusk/internal/config"
	"github.com/hyzr-dev/slusk/internal/observ"
	"github.com/hyzr-dev/slusk/internal/pipeline"
)

type fakeConversationPresenceClient struct {
	usernames []string
	result    map[string]bool
}

func (f *fakeConversationPresenceClient) ConversationPresence(usernames []string) map[string]bool {
	f.usernames = append([]string(nil), usernames...)
	return f.result
}

func TestConversationPresenceForBackend(t *testing.T) {
	fake := &fakeConversationPresenceClient{result: map[string]bool{"alice": true, "bob": false}}
	presence := conversationPresenceForBackend(config.BackendSoulseek, fake)
	if presence == nil {
		t.Fatal("native backend returned nil provider")
	}
	got := presence([]string{"bob", "alice"})
	if len(fake.usernames) != 2 || fake.usernames[0] != "bob" || fake.usernames[1] != "alice" {
		t.Fatalf("delegated usernames = %v", fake.usernames)
	}
	if !got["alice"] || got["bob"] {
		t.Fatalf("delegated result = %v", got)
	}
	if provider := conversationPresenceForBackend(config.BackendSlskd, fake); provider != nil {
		t.Fatal("slskd backend exposed native presence provider")
	}
	if provider := conversationPresenceForBackend(config.BackendSoulseek, nil); provider != nil {
		t.Fatal("nil native client exposed presence provider")
	}
}

func TestDashboardJobsQueryFromPaged(t *testing.T) {
	query := observ.PagedJobsQuery{
		Page:       7,
		Sort:       "updated_at",
		Dir:        "asc",
		Filter:     "failed",
		Source:     "manual",
		Query:      "radiohead",
		PageSize:   25,
		SkipFacets: true,
	}
	before := time.Now()
	got := dashboardJobsQueryFromPaged(query)
	after := time.Now()

	if got.Page != query.Page {
		t.Errorf("Page = %v, want %v", got.Page, query.Page)
	}
	if got.Sort != query.Sort {
		t.Errorf("Sort = %v, want %v", got.Sort, query.Sort)
	}
	if got.Dir != query.Dir {
		t.Errorf("Dir = %v, want %v", got.Dir, query.Dir)
	}
	if got.Filter != query.Filter {
		t.Errorf("Filter = %v, want %v", got.Filter, query.Filter)
	}
	if got.Source != query.Source {
		t.Errorf("Source = %v, want %v", got.Source, query.Source)
	}
	if got.Query != query.Query {
		t.Errorf("Query = %v, want %v", got.Query, query.Query)
	}
	if got.PageSize != query.PageSize {
		t.Errorf("PageSize = %v, want %v", got.PageSize, query.PageSize)
	}
	if got.SkipFacets != query.SkipFacets {
		t.Errorf("SkipFacets = %v, want %v", got.SkipFacets, query.SkipFacets)
	}
	if got.Now.Before(before) || got.Now.After(after) {
		t.Errorf("Now = %v, want between %v and %v", got.Now, before, after)
	}
}

func TestDashboardJobsQueryFromPagedGeneratesFreshNow(t *testing.T) {
	first := dashboardJobsQueryFromPaged(observ.PagedJobsQuery{})
	time.Sleep(time.Millisecond)
	second := dashboardJobsQueryFromPaged(observ.PagedJobsQuery{})

	if !second.Now.After(first.Now) {
		t.Fatalf("second Now (%v) did not advance past first Now (%v)", second.Now, first.Now)
	}
}

type runtimeTestModule struct{}

func (runtimeTestModule) Name() string                          { return "test" }
func (runtimeTestModule) Interval() time.Duration               { return time.Second }
func (runtimeTestModule) Tick(context.Context, time.Time) error { return nil }

type contextRuntimeRunner struct{}

func (contextRuntimeRunner) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

type timeoutRuntimeRunner struct {
	started chan struct{}
	release chan struct{}
}

func (r timeoutRuntimeRunner) Run(ctx context.Context) error {
	go func() { <-r.release }()
	close(r.started)
	<-ctx.Done()
	return pipeline.ErrShutdownTimeout
}

type failingListener struct{ err error }

func (l failingListener) Accept() (net.Conn, error) { return nil, l.err }
func (failingListener) Close() error                { return nil }
func (failingListener) Addr() net.Addr              { return testAddr("failed") }

type testAddr string

func (a testAddr) Network() string { return "test" }
func (a testAddr) String() string  { return string(a) }

func TestListenHTTPReturnsBindFailure(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy address: %v", err)
	}
	defer occupied.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if listener, err := listenHTTP(ctx, occupied.Addr().String()); err == nil {
		listener.Close()
		t.Fatal("listenHTTP succeeded on an occupied address")
	}
}

func TestRunRuntimeTreatsListenerFailureAsFatal(t *testing.T) {
	runner, err := pipeline.NewRunner(slog.New(slog.NewTextHandler(io.Discard, nil)), time.Second, runtimeTestModule{})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	srv := &http.Server{Handler: http.NewServeMux()}
	listenErr := errors.New("accept failed")

	outcome := runRuntime(context.Background(), srv, failingListener{err: listenErr}, runner, time.Second)
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), listenErr.Error()) {
		t.Fatalf("runRuntime error = %v, want listener failure", outcome.err)
	}
	if !outcome.storeCloseSafe {
		t.Fatal("completed runner/server shutdown should permit store close")
	}
	var closed atomic.Bool
	if err := closeStoreAfterRuntime(outcome, func() error { closed.Store(true); return nil }); err != nil {
		t.Fatalf("closeStoreAfterRuntime: %v", err)
	}
	if !closed.Load() {
		t.Fatal("store was not closed after bounded owners completed")
	}
}

func TestRunRuntimeDoesNotCloseStoreWhenModuleMaySurvive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	release := make(chan struct{})
	defer close(release)
	runner := timeoutRuntimeRunner{started: make(chan struct{}), release: release}
	ctx, cancel := context.WithCancel(context.Background())
	outcomes := make(chan runtimeOutcome, 1)
	go func() {
		outcomes <- runRuntime(ctx, &http.Server{Handler: http.NewServeMux()}, listener, runner, 50*time.Millisecond)
	}()
	<-runner.started
	cancel()
	outcome := <-outcomes
	if !errors.Is(outcome.err, pipeline.ErrShutdownTimeout) {
		t.Fatalf("runRuntime error = %v, want ErrShutdownTimeout", outcome.err)
	}
	assertStoreNotClosed(t, outcome)
}

func TestRunRuntimeDoesNotCloseStoreWhenHandlerMaySurvive(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})}
	ctx, cancel := context.WithCancel(context.Background())
	outcomes := make(chan runtimeOutcome, 1)
	go func() { outcomes <- runRuntime(ctx, srv, listener, contextRuntimeRunner{}, 25*time.Millisecond) }()

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	var outcome runtimeOutcome
	select {
	case outcome = <-outcomes:
	case <-time.After(time.Second):
		t.Fatal("runRuntime did not honor bounded HTTP shutdown")
	}
	if outcome.err == nil || !strings.Contains(outcome.err.Error(), "shutdown status server") {
		t.Fatalf("runRuntime error = %v, want server shutdown timeout", outcome.err)
	}
	assertStoreNotClosed(t, outcome)

	releaseOnce.Do(func() { close(release) })
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("blocked request did not finish after release")
	}
}

func assertStoreNotClosed(t *testing.T, outcome runtimeOutcome) {
	t.Helper()
	if outcome.storeCloseSafe {
		t.Fatal("runtime reported store close safe while an owner may survive")
	}
	var closed atomic.Bool
	if err := closeStoreAfterRuntime(outcome, func() error { closed.Store(true); return nil }); err != nil {
		t.Fatalf("closeStoreAfterRuntime: %v", err)
	}
	if closed.Load() {
		t.Fatal("shared store was closed while a runtime owner may survive")
	}
}

func TestHealthcheckURLUsesConfiguredListenerHost(t *testing.T) {
	tests := []struct {
		name       string
		listenAddr string
		want       string
	}{
		{name: "IPv4 wildcard", listenAddr: "0.0.0.0:9090", want: "http://127.0.0.1:9090/healthz"},
		{name: "IPv6 wildcard", listenAddr: "[::]:9090", want: "http://[::1]:9090/healthz"},
		{name: "empty wildcard", listenAddr: ":9090", want: "http://127.0.0.1:9090/healthz"},
		{name: "specific IPv4", listenAddr: "192.0.2.10:9090", want: "http://192.0.2.10:9090/healthz"},
		{name: "specific IPv6", listenAddr: "[2001:db8::10]:9090", want: "http://[2001:db8::10]:9090/healthz"},
		{name: "zoned IPv6", listenAddr: "[fe80::1%eth0]:9090", want: "http://[fe80::1%eth0]:9090/healthz"},
		{name: "hostname", listenAddr: "localhost:9090", want: "http://localhost:9090/healthz"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := healthcheckURL(tt.listenAddr)
			if err != nil {
				t.Fatalf("healthcheckURL: %v", err)
			}
			if got != tt.want {
				t.Fatalf("healthcheckURL(%q) = %q, want %q", tt.listenAddr, got, tt.want)
			}
		})
	}
}

func TestHealthcheckURLRejectsMalformedListener(t *testing.T) {
	if _, err := healthcheckURL("127.0.0.1"); err == nil || !strings.Contains(err.Error(), "observ.listen_addr") {
		t.Fatalf("error = %v, want observ.listen_addr parse error", err)
	}
}

func TestRunHealthcheckProbesConfiguredSpecificListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	fixture, err := os.ReadFile(filepath.Join("..", "..", "internal", "config", "testdata", "valid.toml"))
	if err != nil {
		t.Fatalf("read valid config: %v", err)
	}
	contents := strings.Replace(string(fixture), `listen_addr = "127.0.0.1:9090"`, `listen_addr = "`+listener.Addr().String()+`"`, 1)
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := runHealthcheck(configPath); err != nil {
		t.Fatalf("runHealthcheck: %v", err)
	}
}

func TestEnsureWritableDir(t *testing.T) {
	// A writable dir succeeds and leaves no probe file behind.
	dir := t.TempDir()
	if err := ensureWritableDir(dir); err != nil {
		t.Fatalf("ensureWritableDir(%q) = %v, want nil", dir, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("probe file left behind: %v", entries)
	}

	// A path whose parent is a regular file cannot be created — a
	// user-independent failure (unlike a read-only dir, which root bypasses).
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureWritableDir(filepath.Join(file, "sub")); err == nil {
		t.Error("ensureWritableDir under a regular file = nil, want an error")
	}
}
