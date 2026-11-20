// Package testdb gives integration tests a real PostgreSQL (K22): one container
// per test package, started in TestMain, and one fresh database per test.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/ardaklc20/event-driven-notification-system/migrations"
)

// Image is the PostgreSQL image the tests run. docker-compose.yml names the
// same tag, and TestTestDBImageMatchesCompose keeps the two equal (K38).
const Image = "postgres:18.7-trixie"

const startTimeout = 2 * time.Minute

var (
	baseURL string        // connection string of the container's admin database
	admin   *pgxpool.Pool // used to create and drop the test databases
)

// Run starts the container, runs the package's tests and removes the
// container. Use it as os.Exit(testdb.Run(m)) in TestMain.
func Run(m *testing.M) int {
	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, Image,
		tcpostgres.WithDatabase("postgres"),
		tcpostgres.WithUsername("postgres"),
		tcpostgres.WithPassword("postgres"),
		tcpostgres.BasicWaitStrategies(),
	)
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintf(os.Stderr, "testdb: terminate container: %v\n", err)
		}
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: start %s (is Docker running?): %v\n", Image, err)
		return 1
	}

	baseURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: connection string: %v\n", err)
		return 1
	}
	admin, err = pgxpool.New(ctx, baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "testdb: admin pool: %v\n", err)
		return 1
	}
	defer admin.Close()

	return m.Run()
}

// Empty creates an empty database for the test, drops it when the test ends,
// and returns its connection string.
func Empty(t testing.TB) string {
	t.Helper()
	if admin == nil {
		t.Fatal("testdb: no container; call testdb.Run from TestMain")
	}
	ctx := t.Context()

	name := "test_" + randomHex(t, 8)
	ident := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident); err != nil {
		t.Fatalf("testdb: create database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); err != nil {
			t.Errorf("testdb: drop database %s: %v", name, err)
		}
	})

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("testdb: parse base URL: %v", err)
	}
	u.Path = "/" + name
	return u.String()
}

// New creates a database with every embedded migration applied and returns a
// pool on it. The pool is closed and the database dropped when the test ends.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	dbURL := Empty(t)
	Migrate(t, dbURL)

	pool, err := pgxpool.New(t.Context(), dbURL)
	if err != nil {
		t.Fatalf("testdb: pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Migrate applies the embedded migrations to dbURL.
func Migrate(t testing.TB, dbURL string) {
	t.Helper()
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("testdb: open: %v", err)
	}
	provider, err := migrations.NewProvider(db)
	if err != nil {
		_ = db.Close()
		t.Fatalf("testdb: goose provider: %v", err)
	}
	defer func() { _ = provider.Close() }() // also closes db
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatalf("testdb: migrate up: %v", err)
	}
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("testdb: random name: %v", err)
	}
	return hex.EncodeToString(b)
}
