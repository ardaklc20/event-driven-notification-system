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
	if len(first) != 5 {
		t.Fatalf("first up applied %d migrations, want 5", len(first))
	}
	for i, r := range first {
		if r.Source.Version != int64(i+1) {
			t.Fatalf("migration %d has version %d, want %d", i, r.Source.Version, i+1)
		}
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
			"idempotency_keys_pkey",
			"idempotency_keys_key_length",
			"idempotency_keys_operation_check",
			"idempotency_keys_response_complete",
			"idempotency_keys_notification_id_fkey",
		} {
			var n int
			err := db.QueryRowContext(ctx,
				`SELECT count(*) FROM pg_constraint
				 WHERE conrelid IN ('notifications'::regclass, 'idempotency_keys'::regclass) AND conname = $1`, name).Scan(&n)
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
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
			 VALUES ('+905551234567', 'sms', 'hello', 'normal', 'test')
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

	t.Run("provider result columns are nullable and not unique", func(t *testing.T) {
		for column, typ := range map[string]string{
			"provider_message_id": "text",
			"provider_timestamp":  "timestamp with time zone",
			"sent_at":             "timestamp with time zone",
		} {
			var gotType, nullable string
			err := db.QueryRowContext(ctx,
				`SELECT data_type, is_nullable FROM information_schema.columns
				 WHERE table_name = 'notifications' AND column_name = $1`, column).Scan(&gotType, &nullable)
			if err != nil {
				t.Fatalf("%s: %v", column, err)
			}
			if gotType != typ || nullable != "YES" {
				t.Errorf("%s: %s nullable=%s, want %s nullable=YES", column, gotType, nullable, typ)
			}
		}
		// Two notifications may store the same messageId (K15).
		_, err := db.ExecContext(ctx,
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, provider_message_id)
			 VALUES ('+905551234567', 'sms', 'a', 'normal', 'test', 'same-id'),
			        ('+905551234567', 'sms', 'b', 'normal', 'test', 'same-id')`)
		if err != nil {
			t.Errorf("two rows with the same provider_message_id: %v", err)
		}
	})

	t.Run("unknown enum values are rejected", func(t *testing.T) {
		for name, query := range map[string]string{
			"priority urgent": `INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
				VALUES ('+905551234567', 'sms', 'x', 'urgent', 'test')`,
			"status pending": `INSERT INTO notifications (recipient, channel, content, priority, status, correlation_id)
				VALUES ('+905551234567', 'sms', 'x', 'normal', 'pending', 'test')`,
		} {
			_, err := db.ExecContext(ctx, query)
			assertSQLState(t, name, err, "22P02") // invalid_text_representation
		}
	})

	t.Run("idempotency key rules", func(t *testing.T) {
		for name, query := range map[string]string{
			"empty key":       `INSERT INTO idempotency_keys (key, operation, request_hash) VALUES ('', 'create', '\x00')`,
			"129-byte key":    `INSERT INTO idempotency_keys (key, operation, request_hash) VALUES (repeat('k', 129), 'create', '\x00')`,
			"unknown op":      `INSERT INTO idempotency_keys (key, operation, request_hash) VALUES ('k1', 'delete', '\x00')`,
			"status, no body": `INSERT INTO idempotency_keys (key, operation, request_hash, response_status) VALUES ('k2', 'create', '\x00', 202)`,
		} {
			_, err := db.ExecContext(ctx, query)
			assertSQLState(t, name, err, "23514")
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO idempotency_keys (key, operation, request_hash) VALUES (repeat('k', 128), 'batch', '\x00')`); err != nil {
			t.Errorf("128-byte key rejected: %v", err)
		}
	})

	t.Run("one permit row per channel", func(t *testing.T) {
		var channels string
		var due bool
		err := db.QueryRowContext(ctx,
			`SELECT string_agg(channel, ',' ORDER BY channel), bool_and(next_permit_at <= clock_timestamp())
			 FROM channel_limits`).Scan(&channels, &due)
		if err != nil {
			t.Fatal(err)
		}
		if channels != "email,push,sms" || !due {
			t.Errorf("channel_limits = %s, all due %v; want email,push,sms, all due", channels, due)
		}
		_, err = db.ExecContext(ctx, `INSERT INTO channel_limits (channel, next_permit_at) VALUES ('fax', now())`)
		assertSQLState(t, "channel fax", err, "23514")
	})

	t.Run("claim index is partial on queued rows", func(t *testing.T) {
		var def string
		err := db.QueryRowContext(ctx,
			`SELECT pg_get_indexdef('notifications_claim_idx'::regclass)`).Scan(&def)
		if err != nil {
			t.Fatal(err)
		}
		const want = "CREATE INDEX notifications_claim_idx ON public.notifications USING btree " +
			"(channel, priority, available_at, id) WHERE (status = 'queued'::notification_status)"
		if def != want {
			t.Errorf("index = %s, want %s", def, want)
		}
	})

	t.Run("listing index", func(t *testing.T) {
		var def string
		if err := db.QueryRowContext(ctx,
			`SELECT pg_get_indexdef('notifications_list_idx'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		const want = "CREATE INDEX notifications_list_idx ON public.notifications USING btree (created_at DESC, id DESC)"
		if def != want {
			t.Errorf("index = %s, want %s", def, want)
		}
	})

	t.Run("unknown channel and negative attempts are rejected", func(t *testing.T) {
		for name, query := range map[string]string{
			"channel fax": `INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
				VALUES ('x', 'fax', 'x', 'normal', 'test')`,
			"attempts -1": `INSERT INTO notifications (recipient, channel, content, priority, attempts, correlation_id)
				VALUES ('+905551234567', 'sms', 'x', 'normal', -1, 'test')`,
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
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status, lease_token, lease_expires_at)
			 VALUES ('+905551234567', 'sms', 'x', 'normal', 'test', $1,
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
