package migrations_test

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
	"github.com/ardaklc20/event-driven-notification-system/migrations"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m))
}

// TestMigrations applies the migrations to an empty database twice and checks
// the schema they build (D5, K36, K37).
func TestMigrations(t *testing.T) {
	db, err := sql.Open("pgx", testdb.Empty(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	provider, err := migrations.NewProvider(db)
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	ctx := t.Context()

	first, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("first up: %v", err)
	}
	if len(first) != 1 || first[0].Source.Version != 1 {
		t.Fatalf("first up applied %d migrations, want version 1 only", len(first))
	}

	second, err := provider.Up(ctx)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second up applied %d migrations, want 0", len(second))
	}

	t.Run("enum types in declared order", func(t *testing.T) {
		for typ, want := range map[string]string{
			"notification_priority": "high,normal,low",
			"notification_status":   "queued,processing,sent,failed,cancelled",
		} {
			var got string
			err := db.QueryRowContext(ctx,
				`SELECT string_agg(enumlabel, ',' ORDER BY enumsortorder)
				 FROM pg_enum WHERE enumtypid = $1::regtype`, typ).Scan(&got)
			if err != nil {
				t.Fatalf("%s: %v", typ, err)
			}
			if got != want {
				t.Errorf("%s = %s, want %s", typ, got, want)
			}
		}
	})

	t.Run("constraints exist", func(t *testing.T) {
		for _, name := range []string{
			"notifications_pkey",
			"notifications_channel_check",
			"notifications_attempts_check",
			"notifications_lease_matches_status",
		} {
			var n int
			err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM pg_constraint
				 WHERE conrelid = 'notifications'::regclass AND conname = $1`, name).Scan(&n)
			if err != nil || n != 1 {
				t.Errorf("constraint %s: count=%d err=%v", name, n, err)
			}
		}
	})

	t.Run("generated id is UUIDv7 and defaults apply", func(t *testing.T) {
		var version int
		var status string
		var attempts int
		err := db.QueryRowContext(ctx,
			`INSERT INTO notifications (recipient, channel, content, priority)
			 VALUES ('+905551234567', 'sms', 'hello', 'normal')
			 RETURNING uuid_extract_version(id), status::text, attempts`).Scan(&version, &status, &attempts)
		if err != nil {
			t.Fatalf("insert: %v", err)
		}
		if version != 7 {
			t.Errorf("uuid version = %d, want 7", version)
		}
		if status != "queued" || attempts != 0 {
			t.Errorf("status=%s attempts=%d, want queued 0", status, attempts)
		}
	})

	t.Run("unknown enum values are rejected", func(t *testing.T) {
		for name, query := range map[string]string{
			"priority urgent": `INSERT INTO notifications (recipient, channel, content, priority)
				VALUES ('+905551234567', 'sms', 'x', 'urgent')`,
			"status pending": `INSERT INTO notifications (recipient, channel, content, priority, status)
				VALUES ('+905551234567', 'sms', 'x', 'normal', 'pending')`,
		} {
			_, err := db.ExecContext(ctx, query)
			assertSQLState(t, name, err, "22P02") // invalid_text_representation
		}
	})

	t.Run("unknown channel and negative attempts are rejected", func(t *testing.T) {
		for name, query := range map[string]string{
			"channel fax": `INSERT INTO notifications (recipient, channel, content, priority)
				VALUES ('x', 'fax', 'x', 'normal')`,
			"attempts -1": `INSERT INTO notifications (recipient, channel, content, priority, attempts)
				VALUES ('+905551234567', 'sms', 'x', 'normal', -1)`,
		} {
			_, err := db.ExecContext(ctx, query)
			assertSQLState(t, name, err, "23514") // check_violation
		}
	})
}

// TestLeaseMatchesStatus checks that both lease columns are set exactly when
// the status is processing (K3).
func TestLeaseMatchesStatus(t *testing.T) {
	pool := testdb.New(t)

	type leaseCase struct {
		status   string
		token    bool
		expires  bool
		accepted bool
	}
	var cases []leaseCase
	for _, status := range []string{"processing", "queued"} {
		for _, lease := range [][2]bool{{true, true}, {true, false}, {false, true}, {false, false}} {
			accepted := (status == "processing" && lease[0] && lease[1]) ||
				(status == "queued" && !lease[0] && !lease[1])
			cases = append(cases, leaseCase{status, lease[0], lease[1], accepted})
		}
	}

	rejected := 0
	for _, c := range cases {
		_, err := pool.Exec(t.Context(),
			`INSERT INTO notifications (recipient, channel, content, priority, status, lease_token, lease_expires_at)
			 VALUES ('+905551234567', 'sms', 'x', 'normal', $1,
			         CASE WHEN $2 THEN uuidv7() END,
			         CASE WHEN $3 THEN now() + interval '30 seconds' END)`,
			c.status, c.token, c.expires)
		name := c.status + " token=" + boolText(c.token) + " expires=" + boolText(c.expires)
		if c.accepted {
			if err != nil {
				t.Errorf("%s: rejected, want accepted: %v", name, err)
			}
			continue
		}
		rejected++
		assertConstraint(t, name, err, "notifications_lease_matches_status")
	}
	if rejected != 6 {
		t.Fatalf("checked %d rejected combinations, want 6", rejected)
	}
}

func assertSQLState(t *testing.T, name string, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Errorf("%s: err = %v, want SQLSTATE %s", name, err, code)
	}
}

func assertConstraint(t *testing.T, name string, err error, constraint string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != constraint {
		t.Errorf("%s: err = %v, want check violation of %s", name, err, constraint)
	}
}

func boolText(b bool) string {
	if b {
		return "set"
	}
	return "null"
}
