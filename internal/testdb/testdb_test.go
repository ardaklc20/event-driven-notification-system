package testdb_test

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m))
}

// TestDatabaseIsolation writes a row in one subtest and checks that the next
// subtest, with its own database, does not see it.
func TestDatabaseIsolation(t *testing.T) {
	var writtenID string

	t.Run("writer", func(t *testing.T) {
		pool := testdb.New(t)
		err := pool.QueryRow(t.Context(),
			`INSERT INTO notifications (recipient, channel, content, priority)
			 VALUES ('+905551234567', 'sms', 'hello', 'normal')
			 RETURNING id::text`).Scan(&writtenID)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
	})
	if writtenID == "" {
		t.Fatal("writer subtest wrote no row")
	}

	t.Run("reader", func(t *testing.T) {
		pool := testdb.New(t)
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM notifications`).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count != 0 {
			t.Fatalf("reader sees %d rows, want 0 (row %s leaked from the writer)", count, writtenID)
		}
	})
}

// TestTestDBImageMatchesCompose checks that the postgres service in
// docker-compose.yml names the same image the tests use (K22, K38).
func TestTestDBImageMatchesCompose(t *testing.T) {
	f, err := os.Open("../../docker-compose.yml")
	if err != nil {
		t.Fatalf("open docker-compose.yml: %v", err)
	}
	defer func() { _ = f.Close() }()

	var inPostgres bool
	var image string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 2 && strings.HasSuffix(trimmed, ":"):
			// A service key directly under services:.
			inPostgres = trimmed == "postgres:"
		case indent == 0 && trimmed != "":
			inPostgres = false
		case inPostgres && strings.HasPrefix(trimmed, "image:"):
			image = strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "image:")), `"'`)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read docker-compose.yml: %v", err)
	}
	if image != testdb.Image {
		t.Fatalf("docker-compose.yml postgres image = %q, testdb.Image = %q", image, testdb.Image)
	}
}
