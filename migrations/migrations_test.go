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
	if len(first) != 9 {
		t.Fatalf("first up applied %d migrations, want 9", len(first))
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

	t.Run("provider breaker has one closed row", func(t *testing.T) {
		var (
			rows, generation, failures int
			open, probe                bool
		)
		if err := db.QueryRowContext(ctx,
			`SELECT count(*), max(generation), max(consecutive_failures),
			        bool_or(open_until IS NOT NULL), bool_or(probe_token IS NOT NULL)
			 FROM provider_breaker`).Scan(&rows, &generation, &failures, &open, &probe); err != nil {
			t.Fatal(err)
		}
		if rows != 1 || generation != 0 || failures != 0 || open || probe {
			t.Fatalf("rows=%d generation=%d failures=%d open=%v probe=%v, want one closed row",
				rows, generation, failures, open, probe)
		}
		_, err := db.ExecContext(ctx, `INSERT INTO provider_breaker DEFAULT VALUES`)
		assertSQLState(t, "second row", err, "23505")
		_, err = db.ExecContext(ctx, `INSERT INTO provider_breaker (id) VALUES (false)`)
		assertConstraint(t, "row with key false", err, "provider_breaker_single_row")
		_, err = db.ExecContext(ctx, `UPDATE provider_breaker SET probe_token = gen_random_uuid()`)
		assertConstraint(t, "probe token without its end", err, "provider_breaker_probe_complete")
		_, err = db.ExecContext(ctx,
			`UPDATE provider_breaker SET probe_token = gen_random_uuid(), probe_expires_at = now()`)
		assertConstraint(t, "probe while closed", err, "provider_breaker_probe_only_open")
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

	t.Run("batch schema", func(t *testing.T) {
		var def string
		if err := db.QueryRowContext(ctx,
			`SELECT pg_get_indexdef('notifications_batch_idx'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		const want = "CREATE INDEX notifications_batch_idx ON public.notifications USING btree " +
			"(batch_id, created_at DESC, id DESC) WHERE (batch_id IS NOT NULL)"
		if def != want {
			t.Errorf("index = %s, want %s", def, want)
		}

		var batchID string
		if err := db.QueryRowContext(ctx,
			`INSERT INTO batches (item_count, correlation_id) VALUES (2, 'test') RETURNING id::text`).Scan(&batchID); err != nil {
			t.Fatal(err)
		}
		insert := `INSERT INTO notifications (recipient, channel, content, priority, correlation_id, batch_id, batch_position)
			VALUES ('+905551234567', 'sms', 'x', 'normal', 'test', $1::uuid, $2)`
		if _, err := db.ExecContext(ctx, insert, batchID, 0); err != nil {
			t.Fatalf("first item: %v", err)
		}
		_, err := db.ExecContext(ctx, insert, batchID, 0)
		assertSQLState(t, "same position twice", err, "23505") // unique_violation
		_, err = db.ExecContext(ctx, insert, batchID, nil)
		assertSQLState(t, "batch without position", err, "23514")
		_, err = db.ExecContext(ctx, insert, "01a1f9e9-6831-7926-8408-d50f412669df", 1)
		assertSQLState(t, "unknown batch", err, "23503") // foreign_key_violation
		_, err = db.ExecContext(ctx, `INSERT INTO batches (item_count, correlation_id) VALUES (1001, 'test')`)
		assertSQLState(t, "1,001 items", err, "23514")
	})

	t.Run("completion index", func(t *testing.T) {
		var def string
		if err := db.QueryRowContext(ctx,
			`SELECT pg_get_indexdef('notifications_completed_idx'::regclass)`).Scan(&def); err != nil {
			t.Fatal(err)
		}
		const want = "CREATE INDEX notifications_completed_idx ON public.notifications USING btree " +
			"(completed_at) WHERE (completed_at IS NOT NULL)"
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

// TestCompletedAtBackfill: a sent and a failed row written before migration
// 00007 get their completed_at from it; queued and cancelled rows stay
// without one (K20).
func TestCompletedAtBackfill(t *testing.T) {
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
	if _, err := provider.UpTo(ctx, 6); err != nil {
		t.Fatalf("up to 6: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status, sent_at, updated_at)
		 VALUES ('+905551234567', 'sms', 'sent', 'normal', 'test', 'sent',
		         '2026-11-20T10:00:00Z', '2026-11-20T10:00:05Z'),
		        ('+905551234567', 'sms', 'failed', 'normal', 'test', 'failed',
		         NULL, '2026-11-20T11:00:00Z'),
		        ('+905551234567', 'sms', 'queued', 'normal', 'test', 'queued',
		         NULL, '2026-11-20T12:00:00Z'),
		        ('+905551234567', 'sms', 'cancelled', 'normal', 'test', 'cancelled',
		         NULL, '2026-11-20T13:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up: %v", err)
	}
	want := map[string]string{
		"sent":      "2026-11-20T10:00:00Z",
		"failed":    "2026-11-20T11:00:00Z",
		"queued":    "",
		"cancelled": "",
	}
	for content, at := range want {
		var got sql.NullString
		if err := db.QueryRowContext(ctx,
			`SELECT to_char(completed_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
			 FROM notifications WHERE content = $1`, content).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got.String != at {
			t.Errorf("%s: completed_at = %q, want %q", content, got.String, at)
		}
	}
}
