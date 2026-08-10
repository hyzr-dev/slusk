package main

import (
	"reflect"
	"testing"

	"github.com/hyzr-dev/slusk/internal/store"
)

// TestJobStatusFacetsCopiesEveryField catches the failure mode that shipped
// once already in issue #470: the facet existed on the store side, existed on
// the wire side, was accepted by both filter allowlists, had a chip in the
// dashboard - and read 0 forever, because the hand-written copy between the two
// structs never learned about it. Nothing failed. Nothing could.
//
// internal/observ declaring its own types instead of importing the store's is
// deliberate, so this copy is the price of that boundary rather than a mistake
// to remove. What is fixable is that the compiler cannot check it, which is
// what this test stands in for.
//
// Every source field is set to a distinct non-zero value and every destination
// field must come back non-zero and distinct. Distinct matters as much as
// non-zero: a copy that assigns the wrong source field to a destination would
// otherwise pass. Fields are enumerated by reflection, so a facet added later
// is covered without anyone remembering to extend a list here - the direction a
// hand-written list always misses.
func TestJobStatusFacetsCopiesEveryField(t *testing.T) {
	var src store.DashboardStatusFacets
	v := reflect.ValueOf(&src).Elem()
	if v.NumField() == 0 {
		t.Fatal("DashboardStatusFacets has no fields; this test would pass vacuously")
	}
	for i := range v.NumField() {
		if v.Field(i).Kind() != reflect.Int64 {
			t.Fatalf("%s is %s, not int64; this test's distinct-value scheme assumes counts",
				v.Type().Field(i).Name, v.Field(i).Kind())
		}
		v.Field(i).SetInt(int64(i + 1))
	}

	got := reflect.ValueOf(jobStatusFacets(src))
	// Matched by NAME, not by position: the two structs are free to list their
	// fields in different orders, and a positional check would both fail on a
	// harmless reorder and pass on a genuinely crossed assignment.
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		dst := got.FieldByName(name)
		if !dst.IsValid() {
			t.Errorf("store has facet %s and the wire shape has no such field, so it can never reach the dashboard", name)
			continue
		}
		want := v.Field(i).Int()
		switch n := dst.Int(); {
		case n == 0:
			t.Errorf("%s is never copied, so the dashboard shows 0 for it whatever the database says", name)
		case n != want:
			t.Errorf("%s = %d, want %d — it is copied from the wrong source field", name, n, want)
		}
	}
}
