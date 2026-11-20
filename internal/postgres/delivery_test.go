package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

const testLease = 30 * time.Second

// insertQueued inserts n queued notifications and returns their IDs.
func insertQueued(t *testing.T, pool *pgxpool.Pool, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	rows, err := pool.Query(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id)
		 SELECT '+905551234567', 'sms', 'hello ' || g, 'normal', 'corr-' || g
		 FROM generate_series(1, $1) AS g
		 RETURNING id::text`, n)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func mustClaim(t *testing.T, store *postgres.Store) domain.Claim {
	t.Helper()
	c, ok, err := store.ClaimNext(t.Context(), testLease)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	return c
}

type leaseRow struct {
	status    string
	token     *string
	expires   *time.Time
	attempts  int
	sentAt    *time.Time
	messageID *string
	timestamp *time.Time
	lastError *string
}

func readLease(t *testing.T, pool *pgxpool.Pool, id string) leaseRow {
	t.Helper()
	var r leaseRow
	err := pool.QueryRow(t.Context(),
		`SELECT status::text, lease_token::text, lease_expires_at, attempts, sent_at,
		        provider_message_id, provider_timestamp, last_error
		 FROM notifications WHERE id = $1::uuid`, id).Scan(
		&r.status, &r.token, &r.expires, &r.attempts, &r.sentAt, &r.messageID, &r.timestamp, &r.lastError)
	if err != nil {
		t.Fatalf("read row %s: %v", id, err)
	}
	return r
}

// TestClaimSetsLease claims one queued row (K3, K9).
func TestClaimSetsLease(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	id := insertQueued(t, pool, 1)[0]

	c := mustClaim(t, store)
	if c.ID != id || c.Attempt != 1 || c.LeaseToken == "" || c.CorrelationID != "corr-1" ||
		c.Recipient != "+905551234567" || c.Channel != domain.ChannelSMS || c.Content != "hello 1" {
		t.Fatalf("claim = %+v", c)
	}
	if c.LeaseRemaining <= 0 || c.LeaseRemaining > testLease {
		t.Fatalf("remaining lease %s, want in (0, %s]", c.LeaseRemaining, testLease)
	}

	r := readLease(t, pool, id)
	if r.status != "processing" || r.token == nil || *r.token != c.LeaseToken || r.attempts != 1 {
		t.Fatalf("row = %+v, want processing with the claim's token and attempts 1", r)
	}
	var inFuture bool
	if err := pool.QueryRow(t.Context(),
		`SELECT lease_expires_at > clock_timestamp() FROM notifications WHERE id = $1::uuid`, id).Scan(&inFuture); err != nil || !inFuture {
		t.Fatalf("lease expiry in the future = %v (err %v)", inFuture, err)
	}

	if _, ok, err := store.ClaimNext(t.Context(), testLease); ok || err != nil {
		t.Fatalf("second claim on an empty queue: ok=%v err=%v", ok, err)
	}
}

// TestSingleLease runs two claimers on separate pools until the queue is
// empty. Every row must be claimed exactly once (R11, N2, K2).
func TestSingleLease(t *testing.T) {
	pool := testdb.New(t)
	other, err := pgxpool.New(t.Context(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)

	const rows = 200
	insertQueued(t, pool, rows)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	claims := make([][]domain.Claim, 2)
	errs := make([]error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, p := range []*pgxpool.Pool{pool, other} {
		store := postgres.NewStore(p)
		wg.Go(func() {
			<-start
			for {
				c, ok, err := store.ClaimNext(ctx, testLease)
				if err != nil {
					errs[i] = err
					return
				}
				if !ok {
					return
				}
				claims[i] = append(claims[i], c)
			}
		})
	}
	close(start)
	wg.Wait()

	ids := map[string]int{}
	tokens := map[string]bool{}
	for i := range claims {
		if errs[i] != nil {
			t.Fatalf("claimer %d: %v", i, errs[i])
		}
		for _, c := range claims[i] {
			ids[c.ID]++
			tokens[c.LeaseToken] = true
		}
	}
	t.Logf("claimer 0 took %d rows, claimer 1 took %d", len(claims[0]), len(claims[1]))
	if len(ids) != rows || len(tokens) != rows {
		t.Fatalf("%d distinct rows and %d distinct tokens claimed, want %d each", len(ids), len(tokens), rows)
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("row %s claimed %d times", id, n)
		}
	}
	var processing, onceAttempted int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FILTER (WHERE status = 'processing'), count(*) FILTER (WHERE attempts = 1)
		 FROM notifications`).Scan(&processing, &onceAttempted); err != nil {
		t.Fatal(err)
	}
	if processing != rows || onceAttempted != rows {
		t.Fatalf("processing=%d attempts=1 on %d rows, want %d each", processing, onceAttempted, rows)
	}
}

// TestResultRequiresValidLease checks the K3 conditions of every result write:
// the token, the lease expiry by the database clock and the processing status.
func TestResultRequiresValidLease(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ctx := t.Context()
	id := insertQueued(t, pool, 1)[0]
	c := mustClaim(t, store)
	acc := domain.Acceptance{MessageID: "m-1", Timestamp: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)}

	exec := func(query string) {
		t.Helper()
		if _, err := pool.Exec(ctx, query, id); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	unchanged := func(name string, changed bool, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if changed {
			t.Fatalf("%s: a row changed, want none", name)
		}
	}

	// Wrong token.
	var wrongToken string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&wrongToken); err != nil {
		t.Fatal(err)
	}
	changed, err := store.MarkSent(ctx, id, wrongToken, acc)
	unchanged("sent with a wrong token", changed, err)
	changed, err = store.MarkFailed(ctx, id, wrongToken, "x")
	unchanged("failed with a wrong token", changed, err)

	// The lease expired by the database clock, token still right.
	exec(`UPDATE notifications SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1::uuid`)
	changed, err = store.MarkSent(ctx, id, c.LeaseToken, acc)
	unchanged("sent after expiry", changed, err)
	changed, err = store.MarkFailed(ctx, id, c.LeaseToken, "x")
	unchanged("failed after expiry", changed, err)
	exec(`UPDATE notifications SET lease_expires_at = clock_timestamp() + interval '30 seconds' WHERE id = $1::uuid`)

	// The row left processing, as recovery would do it (slice 4).
	exec(`UPDATE notifications SET status = 'queued', lease_token = NULL, lease_expires_at = NULL WHERE id = $1::uuid`)
	changed, err = store.MarkSent(ctx, id, c.LeaseToken, acc)
	unchanged("sent after the row left processing", changed, err)
	if r := readLease(t, pool, id); r.status != "queued" || r.sentAt != nil {
		t.Fatalf("row after rejected writes = %+v, want queued and never sent", r)
	}

	// A valid lease: one row changes and both lease columns are cleared.
	c = mustClaim(t, store)
	changed, err = store.MarkSent(ctx, id, c.LeaseToken, acc)
	if err != nil || !changed {
		t.Fatalf("valid sent: changed=%v err=%v", changed, err)
	}
	r := readLease(t, pool, id)
	if r.status != "sent" || r.token != nil || r.expires != nil || r.sentAt == nil ||
		r.messageID == nil || *r.messageID != "m-1" || r.timestamp == nil || !r.timestamp.Equal(acc.Timestamp) {
		t.Fatalf("row = %+v, want sent with its provider fields and no lease", r)
	}

	// The failed write follows the same rule.
	failedID := insertQueued(t, pool, 1)[0]
	fc := mustClaim(t, store)
	if fc.ID != failedID {
		t.Fatalf("claimed %s, want %s", fc.ID, failedID)
	}
	changed, err = store.MarkFailed(ctx, failedID, fc.LeaseToken, "unexpected status 307")
	if err != nil || !changed {
		t.Fatalf("valid failed: changed=%v err=%v", changed, err)
	}
	fr := readLease(t, pool, failedID)
	if fr.status != "failed" || fr.token != nil || fr.expires != nil || fr.lastError == nil || *fr.lastError != "unexpected status 307" {
		t.Fatalf("row = %+v, want failed with last_error and no lease", fr)
	}
}

// TestDuplicateMessageID stores the same provider messageId on two rows (K15).
func TestDuplicateMessageID(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ids := insertQueued(t, pool, 2)
	acc := domain.Acceptance{MessageID: "static-webhook-site-id"}

	for range ids {
		c := mustClaim(t, store)
		changed, err := store.MarkSent(t.Context(), c.ID, c.LeaseToken, acc)
		if err != nil || !changed {
			t.Fatalf("mark sent %s: changed=%v err=%v", c.ID, changed, err)
		}
	}
	for _, id := range ids {
		r := readLease(t, pool, id)
		if r.status != "sent" || r.messageID == nil || *r.messageID != acc.MessageID || r.timestamp != nil {
			t.Fatalf("row %s = %+v, want sent with the shared messageId and no timestamp", id, r)
		}
	}
}
