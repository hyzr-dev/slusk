package observ

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/hyzr-dev/slusk/internal/core"
	"github.com/hyzr-dev/slusk/internal/store"
)

// TestPeersSortKeysMatchTheStore is the cross-package half of the #310 guard.
// internal/observ cannot import internal/store in production code, so
// parsePeersQuery necessarily holds a second copy of the sort allowlist; a key
// the store accepts but this parser rejects is a 400 that store-level tests
// cannot see, and vice versa is a 500. The test-only import is the cheapest
// way to make the two copies fail together instead of silently apart.
func TestPeersSortKeysMatchTheStore(t *testing.T) {
	for _, key := range store.PeersSortKeys {
		if _, err := parsePeersQuery(&url.URL{RawQuery: "sort=" + key}); err != nil {
			t.Errorf("parsePeersQuery rejected sort key %q that the store accepts: %v", key, err)
		}
	}
	// The other direction: anything this parser accepts must be in the store's
	// list, or the request 500s at the store instead of 400ing here.
	for _, key := range []string{"score", "successCount", "failCount", "username"} {
		found := false
		for _, known := range store.PeersSortKeys {
			if known == key {
				found = true
			}
		}
		if !found {
			t.Errorf("parsePeersQuery accepts sort key %q the store does not know", key)
		}
	}
}

// TestPeersPageSizeBoundsMatchTheStore keeps the two page-size guards in step
// for the same reason: a size this layer waves through and the store rejects
// is a 500 on a request the user could have been told about.
func TestPeersPageSizeBoundsMatchTheStore(t *testing.T) {
	if peersPageSize != store.PeersPageSize {
		t.Errorf("default page size = %d, store's = %d", peersPageSize, store.PeersPageSize)
	}
	if peersPageSizeMin != store.PeersPageSizeMin || peersPageSizeMax != store.PeersPageSizeMax {
		t.Errorf("bounds = [%d, %d], store's = [%d, %d]",
			peersPageSizeMin, peersPageSizeMax, store.PeersPageSizeMin, store.PeersPageSizeMax)
	}
}

func TestParsePeersQuery(t *testing.T) {
	defaults := PeersQuery{Sort: "score", Dir: "desc", PageSize: peersPageSize}

	t.Run("defaults", func(t *testing.T) {
		got, err := parsePeersQuery(&url.URL{})
		if err != nil {
			t.Fatalf("parsePeersQuery: %v", err)
		}
		if got != defaults {
			t.Errorf("got %+v, want %+v", got, defaults)
		}
	})

	t.Run("accepted", func(t *testing.T) {
		got, err := parsePeersQuery(&url.URL{RawQuery: "page=3&pageSize=10&sort=username&dir=asc"})
		if err != nil {
			t.Fatalf("parsePeersQuery: %v", err)
		}
		want := PeersQuery{Page: 3, PageSize: 10, Sort: "username", Dir: "asc"}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	for _, raw := range []string{
		"sort=lastSeen",
		"dir=sideways",
		"pageSize=0",
		"pageSize=51",
		"pageSize=notanumber",
		"page=-1",
		"page=notanumber",
		"filter=all", // a jobs-list parameter this endpoint does not serve
		"page=1&page=2",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parsePeersQuery(&url.URL{RawQuery: raw}); err == nil {
				t.Errorf("parsePeersQuery accepted %q", raw)
			}
		})
	}
}

// TestPeersEndpointRejectsBadParamsWithBadRequest pins the status code: a
// malformed sort key is the caller's mistake, and answering 500 would send
// someone looking for a broken store.
func TestPeersEndpointRejectsBadParamsWithBadRequest(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.Peers = func(ctx context.Context, query PeersQuery) (PeersResult, error) {
		t.Error("GET /api/peers reached the store with an invalid query")
		return PeersResult{}, nil
	}
	h := NewServer(deps)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/peers?sort=lastSeen", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestPeersEndpointForwardsTheQuery: the parsed page is what the store is
// asked for. Without this the endpoint could parse perfectly and still serve
// page 0 forever.
func TestPeersEndpointForwardsTheQuery(t *testing.T) {
	var got PeersQuery
	deps := testServerDeps(prometheus.NewRegistry())
	deps.Peers = func(ctx context.Context, query PeersQuery) (PeersResult, error) {
		got = query
		return PeersResult{Total: 7}, nil
	}
	h := NewServer(deps)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/peers?page=2&pageSize=5&sort=failCount&dir=asc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	want := PeersQuery{Page: 2, PageSize: 5, Sort: "failCount", Dir: "asc"}
	if got != want {
		t.Errorf("store received %+v, want %+v", got, want)
	}
}

// TestPeersEndpointServesAnEmptyPageAsAnArray: an out-of-range page is an
// empty page with the real total. `peers: null` would make every consumer
// handle a case the endpoint never needs to produce.
func TestPeersEndpointServesAnEmptyPageAsAnArray(t *testing.T) {
	deps := testServerDeps(prometheus.NewRegistry())
	deps.Peers = func(ctx context.Context, query PeersQuery) (PeersResult, error) {
		return PeersResult{Peers: nil, Total: 3}, nil
	}
	h := NewServer(deps)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/peers?page=99", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Peers []core.PeerRow `json:"peers"`
		Total int64          `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Peers == nil {
		t.Errorf("peers = null, want []: %s", rec.Body.String())
	}
	if envelope.Total != 3 {
		t.Errorf("total = %d, want 3", envelope.Total)
	}
}
