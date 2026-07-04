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

	dir, err := os.MkdirTemp("", "slskdarr-pgtest-*")
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

	name := fmt.Sprintf("slskdarr_test_%d", dbCount.Add(1))
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatalf("storetest: create database %s: %v", name, err)
	}
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/%s?sslmode=disable", port, name)
}

func adminDSN() string {
	return fmt.Sprintf("postgres://postgres:postgres@localhost:%d/postgres?sslmode=disable", port)
}
