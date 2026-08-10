package observ

import (
	"net/url"
	"reflect"
	"testing"
)

// TestEveryStatusFacetIsAnAcceptableFilter closes a hole that has now cost this
// repo twice.
//
// The dashboard's status chips are built from JobStatusFacets, and clicking one
// sends its name as ?filter=. But the accepted filter values live in a second,
// hand-written allowlist inside parsePagedJobsQuery - and a third copy inside
// store.validateDashboardJobsQuery. Adding a status to the facets without
// adding it to the allowlist gives a chip that 400s the moment anyone clicks
// it, and the comment above that allowlist records issue #310 shipping exactly
// that until a lab run caught it.
//
// Both halves were correct when this test was written; that is not the point.
// Deleting "importRefused" from the allowlist leaves every other test in this
// repo green, which is the property that let the bug ship. This test fails
// instead.
//
// The expectation is derived from the struct's JSON tags rather than a list
// written out here, because a hand-written list is exactly the thing that goes
// stale: it would catch a removal and never catch an addition, which is the
// direction that actually breaks the interface.
func TestEveryStatusFacetIsAnAcceptableFilter(t *testing.T) {
	facets := reflect.TypeOf(JobStatusFacets{})
	if facets.NumField() == 0 {
		t.Fatal("JobStatusFacets has no fields; this test would pass vacuously")
	}
	for i := range facets.NumField() {
		name := facets.Field(i).Tag.Get("json")
		if name == "" || name == "-" {
			t.Errorf("%s has no json tag, so the client cannot name it", facets.Field(i).Name)
			continue
		}
		u := &url.URL{RawQuery: url.Values{"filter": {name}}.Encode()}
		if _, err := parsePagedJobsQuery(u); err != nil {
			t.Errorf("filter=%q is a status chip the dashboard renders but the API rejects it: %v", name, err)
		}
	}
}
