package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samuelenocsson/slskdarr/internal/pipeline"
)

type runtimeTestModule struct{}

func (runtimeTestModule) Name() string                          { return "test" }
func (runtimeTestModule) Interval() time.Duration               { return time.Second }
func (runtimeTestModule) Tick(context.Context, time.Time) error { return nil }

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

	err = runRuntime(context.Background(), srv, failingListener{err: listenErr}, runner)
	if err == nil || !strings.Contains(err.Error(), listenErr.Error()) {
		t.Fatalf("runRuntime error = %v, want listener failure", err)
	}
}
