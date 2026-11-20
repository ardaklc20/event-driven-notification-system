package postgres_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func readVersion(t *testing.T, pool *pgxpool.Pool, id string) (string, int64) {
	t.Helper()
	var (
		status  string
		version int64
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT status::text, version FROM notifications WHERE id = $1::uuid`, id).Scan(&status, &version); err != nil {
		t.Fatalf("read version of %s: %v", id, err)
	}
	return status, version
}

func wantVersion(t *testing.T, pool *pgxpool.Pool, id, step, wantStatus string, want int64) {
	t.Helper()
	status, version := readVersion(t, pool, id)
	if status != wantStatus || version != want {
		t.Fatalf("after %s: status %s version %d, want %s version %d", step, status, version, wantStatus, want)
	}
}

// TestStatusVersionIncrements: every path that changes a status adds exactly
// 1 to version; updates that leave the status as it was do not (K24).
func TestStatusVersionIncrements(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ctx := t.Context()

	// Claim, retry, claim, result.
	a := insertQueued(t, pool, 1)[0]
	wantVersion(t, pool, a, "insert", "queued", 0)
	c := mustClaim(t, store)
	wantVersion(t, pool, a, "claim", "processing", 1)
	if ok, err := store.MarkRetry(ctx, a, c.LeaseToken, "temporary: status 503", 0); !ok || err != nil {
		t.Fatalf("retry: ok=%v err=%v", ok, err)
	}
	wantVersion(t, pool, a, "retry", "queued", 2)
	c = mustClaim(t, store)
	wantVersion(t, pool, a, "second claim", "processing", 3)
	if _, err := pool.Exec(ctx, `UPDATE notifications SET lease_expires_at = lease_expires_at + interval '1 second' WHERE id = $1::uuid`, a); err != nil {
		t.Fatal(err)
	}
	wantVersion(t, pool, a, "an update without a status change", "processing", 3)
	if ok, err := store.MarkSent(ctx, a, c.LeaseToken, domain.Acceptance{}); !ok || err != nil {
		t.Fatalf("sent: ok=%v err=%v", ok, err)
	}
	wantVersion(t, pool, a, "result", "sent", 4)
	if _, err := pool.Exec(ctx, `UPDATE notifications SET version = 99, last_error = 'x' WHERE id = $1::uuid`, a); err != nil {
		t.Fatal(err)
	}
	wantVersion(t, pool, a, "a direct write of version", "sent", 4)

	// Cancel.
	b := insertQueued(t, pool, 1)[0]
	if _, err := store.CancelNotification(ctx, b); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	wantVersion(t, pool, b, "cancel", "cancelled", 1)

	// Lease recovery.
	r := insertQueued(t, pool, 1)[0]
	mustClaim(t, store)
	wantVersion(t, pool, r, "claim", "processing", 1)
	expire(t, pool, r)
	if got := recoverAll(t, store); len(got) != 1 {
		t.Fatalf("recovered %d rows, want 1", len(got))
	}
	wantVersion(t, pool, r, "recovery", "queued", 2)
}

// listenConn takes a connection out of pool that listens on the status
// channel.
func listenConn(t *testing.T, pool *pgxpool.Pool) *pgx.Conn {
	t.Helper()
	pc, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	conn := pc.Hijack()
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(t.Context(), "LISTEN "+postgres.StatusChannel); err != nil {
		t.Fatal(err)
	}
	return conn
}

func waitNotification(t *testing.T, conn *pgx.Conn, d time.Duration) (string, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	defer cancel()
	n, err := conn.WaitForNotification(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return "", false
		}
		t.Fatalf("wait: %v", err)
	}
	return n.Payload, true
}

// TestNotifyOnCommitOnly: a status change that rolls back sends nothing; a
// committed one arrives (K24).
func TestNotifyOnCommitOnly(t *testing.T) {
	pool := testdb.New(t)
	ids := insertQueued(t, pool, 2)
	listener := listenConn(t, pool)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE notifications SET status = 'cancelled' WHERE id = $1::uuid`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE notifications SET status = 'cancelled' WHERE id = $1::uuid`, ids[1]); err != nil {
		t.Fatal(err)
	}

	payload, ok := waitNotification(t, listener, 5*time.Second)
	if !ok {
		t.Fatal("the committed change sent no notification")
	}
	var got struct {
		NotificationID string `json:"notification_id"`
	}
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatal(err)
	}
	if got.NotificationID != ids[1] {
		t.Fatalf("first notification is for %s, want the committed %s (rolled back: %s)", got.NotificationID, ids[1], ids[0])
	}
	if extra, ok := waitNotification(t, listener, 500*time.Millisecond); ok {
		t.Fatalf("unexpected second notification: %s", extra)
	}
}

// TestNotifyPayload: a batch item's event carries the six fields with the
// row's values, far below the 8000-byte limit (K24).
func TestNotifyPayload(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ctx := t.Context()
	if _, err := store.CreateBatch(ctx, batchCommand("payload", "a", "b")); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM notifications WHERE batch_position = 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	listener := listenConn(t, pool)
	if _, err := store.CancelNotification(ctx, id); err != nil {
		t.Fatal(err)
	}

	payload, ok := waitNotification(t, listener, 5*time.Second)
	if !ok {
		t.Fatal("no notification")
	}
	if len(payload) >= 8000 {
		t.Fatalf("payload is %d bytes, want below 8000", len(payload))
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		t.Fatal(err)
	}
	keys := slices.Sorted(maps.Keys(fields))
	want := []string{"attempts", "batch_id", "notification_id", "status", "updated_at", "version"}
	if !slices.Equal(keys, want) {
		t.Fatalf("payload fields %v, want %v", keys, want)
	}

	var p struct {
		NotificationID string    `json:"notification_id"`
		BatchID        string    `json:"batch_id"`
		Status         string    `json:"status"`
		Attempts       int       `json:"attempts"`
		Version        int64     `json:"version"`
		UpdatedAt      time.Time `json:"updated_at"`
	}
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		t.Fatal(err)
	}
	var (
		batchID, status string
		attempts        int
		version         int64
		updatedAt       time.Time
	)
	if err := pool.QueryRow(ctx,
		`SELECT batch_id::text, status::text, attempts, version, updated_at FROM notifications WHERE id = $1::uuid`, id).
		Scan(&batchID, &status, &attempts, &version, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if p.NotificationID != id || p.BatchID != batchID || p.Status != status || p.Attempts != attempts ||
		p.Version != version || !p.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("payload %s\nrow: batch %s status %s attempts %d version %d updated_at %s",
			payload, batchID, status, attempts, version, updatedAt.Format(time.RFC3339Nano))
	}
	t.Logf("payload: %d bytes", len(payload))
}

// sinkSpy records what the listener reports.
type sinkSpy struct {
	mu        sync.Mutex
	connected chan uint64
	events    chan domain.StatusEvent
	gen       uint64
}

func newSinkSpy() *sinkSpy {
	return &sinkSpy{connected: make(chan uint64, 10), events: make(chan domain.StatusEvent, 10)}
}

func (s *sinkSpy) Connected() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.connected <- s.gen
	return s.gen
}
func (s *sinkSpy) Disconnected()                 {}
func (s *sinkSpy) Publish(ev domain.StatusEvent) { s.events <- ev }

// TestListenerOutsideTransaction: the waiting listener's backend has no open
// transaction, so it cannot hold back cleanup of the notification queue (K24).
func TestListenerOutsideTransaction(t *testing.T) {
	pool := testdb.New(t)
	spy := newSinkSpy()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		postgres.NewListener(pool, spy, slog.New(slog.DiscardHandler)).Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-spy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not connect")
	}

	// One event passes through, so the backend has handled a notification
	// before it is inspected.
	id := insertQueued(t, pool, 1)[0]
	if _, err := pool.Exec(t.Context(), `UPDATE notifications SET status = 'cancelled' WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-spy.events:
		if ev.NotificationID != id || ev.Status != domain.StatusCancelled || ev.Version != 1 {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}

	var (
		backends  int
		xactStart *time.Time
		state     string
	)
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) OVER (), xact_start, state FROM pg_stat_activity
		 WHERE datname = current_database() AND query = 'LISTEN notification_status'`).
		Scan(&backends, &xactStart, &state); err != nil {
		t.Fatalf("find the listener's backend: %v", err)
	}
	if backends != 1 || xactStart != nil || state != "idle" {
		t.Fatalf("listener backend: count %d, xact_start %v, state %s; want one idle backend with a null xact_start",
			backends, xactStart, state)
	}
}
