package store

import (
	"path/filepath"
	"testing"
)

// newTestStore opens a fresh store in a temp dir, closed automatically.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenAppliesSchema(t *testing.T) {
	s := newTestStore(t)
	var count int
	err := s.db.QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='album_jobs'`,
	).Scan(&count)
	if err != nil {
		t.Fatalf("query schema: %v", err)
	}
	if count != 1 {
		t.Errorf("album_jobs table not created")
	}
}
