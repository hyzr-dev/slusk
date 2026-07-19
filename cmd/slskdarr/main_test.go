package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
