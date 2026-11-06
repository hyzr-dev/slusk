// Package storetest runs one embedded PostgreSQL instance for a test package
// and hands out a fresh, uniquely-named database per test. Use it from a
// package's TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(storetest.Run(m)) }
//
// and from individual tests via DSN(t). The first run downloads the Postgres
// binaries into ~/.embedded-postgres-go, so it can take a while.
package storetest

import (
	"database/sql"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	port    uint32
	dbCount atomic.Int64
)

// Run starts the package-wide embedded Postgres, runs the tests, and stops it.
// Return its result from TestMain via os.Exit.
func Run(m *testing.M) int {
	// A non-default randomized port avoids clashing with a locally running
	// Postgres (5432) or another test package's instance.
	port = uint32(15432 + rand.Intn(10000))

	dir, err := os.MkdirTemp("", "slusk-pgtest-*")
	if err != nil {
		log.Printf("storetest: temp dir: %v", err)
		return 1
	}
	defer os.RemoveAll(dir)

	pg := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
		Port(port).
		RuntimePath(filepath.Join(dir, "runtime")).
		DataPath(filepath.Join(dir, "data")).
		Logger(io.Discard))
	if err := pg.Start(); err != nil {
		log.Printf("storetest: start embedded postgres: %v", err)
		return 1
	}
	defer func() {
		if err := pg.Stop(); err != nil {
			log.Printf("storetest: stop embedded postgres: %v", err)
		}
	}()

	return m.Run()
}

// DSN creates a fresh, uniquely-named database on the package's embedded
// Postgres instance and returns a DSN pointing at it. Each call gets its own
// database, so tests stay isolated without dropping tables between them.
func DSN(t testing.TB) string {
	t.Helper()
	admin, err := sql.Open("pgx", adminDSN())
	if err != nil {
		t.Fatalf("storetest: open admin connection: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("slusk_test_%d", dbCount.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("storetest: create database %s: %v", name, err)
	}
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/%s?sslmode=disable", port, name)
}

func adminDSN() string {
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
}

// JobCycle names the search-cycle columns of an album_jobs row for SeedJobCycle.
// A nil field is left as it is.
type JobCycle struct {
	Retries       *int
	EmptySearches *int
	NotBefore     *time.Time
}

// SeedJobCycle writes the given search-cycle columns of a job with raw SQL,
// whatever state the job is in, and reads each back. The store's writers for
// these columns (SetJobBackoff, SetJobEmptySearchBackoff) are guarded on
// WANTED, so on a job seeded in any other state they silently no-op; a reset
// test that then asserts the column is zero or nil would pass vacuously. The
// read-back fails the test if the seed did not take.
func SeedJobCycle(t testing.TB, dsn string, jobID int64, c JobCycle) {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("storetest: SeedJobCycle open: %v", err)
	}
	defer db.Close()

	if c.Retries != nil {
		seedColumn(t, db, jobID, "retries", *c.Retries)
	}
	if c.EmptySearches != nil {
		seedColumn(t, db, jobID, "empty_searches", *c.EmptySearches)
	}
	if c.NotBefore != nil {
		seedColumn(t, db, jobID, "not_before", *c.NotBefore)
	}
}

// seedColumn sets one album_jobs column and checks the database holds the value
// afterwards. column is always a literal from SeedJobCycle, never caller input.
func seedColumn[V int | time.Time](t testing.TB, db *sql.DB, jobID int64, column string, want V) {
	t.Helper()
	if _, err := db.Exec(`UPDATE album_jobs SET `+column+` = $1 WHERE id = $2`, want, jobID); err != nil {
		t.Fatalf("storetest: seed %s: %v", column, err)
	}
	var got V
	if err := db.QueryRow(`SELECT `+column+` FROM album_jobs WHERE id = $1`, jobID).Scan(&got); err != nil {
		t.Fatalf("storetest: read back %s: %v", column, err)
	}
	if !equalSeed(got, want) {
		t.Fatalf("storetest: seed %s = %v, want %v: the seed did not take", column, got, want)
	}
}

func equalSeed[V int | time.Time](a, b V) bool {
	if ta, ok := any(a).(time.Time); ok {
		return ta.Equal(any(b).(time.Time))
	}
	return a == b
}
