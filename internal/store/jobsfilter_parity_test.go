package store

import (
	"reflect"
	"testing"
	"unicode"
)

// TestEveryStatusFacetIsAnAcceptableFilter is the store-side twin of the test
// of the same name in internal/observ.
//
// There are THREE hand-written lists over the same set of status names: the
// facets struct that produces the dashboard's chips, observ's filter
// allowlist, and this package's. A request has to clear both allowlists, and
// they fail differently - a value this one refuses but observ accepts 500s
// instead of 400ing, which the comment above observ's copy already records.
// Covering only observ's would leave the half that fails louder unguarded.
//
// Consolidating the registries is deliberately out of scope for issue #470;
// this is the cheap standing check that they have not drifted in the meantime.
//
// The names come from the struct's own fields rather than a list written here,
// because a hand-written list catches a removal and never catches an addition -
// and an addition is what breaks a chip.
func TestEveryStatusFacetIsAnAcceptableFilter(t *testing.T) {
	facets := reflect.TypeOf(DashboardStatusFacets{})
	if facets.NumField() == 0 {
		t.Fatal("DashboardStatusFacets has no fields; this test would pass vacuously")
	}
	for i := range facets.NumField() {
		filter := lowerFirst(facets.Field(i).Name)
		q := DashboardJobsQuery{Filter: filter, Source: "all", Sort: "st", Dir: "asc", PageSize: 50}
		if err := validateDashboardJobsQuery(q); err != nil {
			t.Errorf("filter=%q is a status chip the dashboard renders but the store rejects it: %v", filter, err)
		}
	}
}

// lowerFirst maps a facet's Go field name to the wire name the client sends.
// This side's struct carries no json tags, so the mapping is a convention
// rather than something declared - observ.JobStatusFacets' tags are where it is
// written down, and its own copy of this test checks that half.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToLower(r[0])
	return string(r)
}
