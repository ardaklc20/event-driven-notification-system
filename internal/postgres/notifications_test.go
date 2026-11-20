package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m))
}

var errLimit = errors.New("over the content limit")

func command(key string, checkLimits func() error) domain.CreateCommand {
	n := domain.NewNotification{Recipient: "+905551234567", Channel: domain.ChannelSMS, Content: "hello", Priority: domain.PriorityNormal}
	cmd := domain.CreateCommand{
		Operation:     domain.OperationCreate,
		Notification:  n,
		CorrelationID: "test",
		Key:           key,
		RequestHash:   domain.RequestHash(domain.OperationCreate, []domain.NewNotification{n}),
		Respond: func(created domain.Notification) ([]byte, error) {
			return json.Marshal(map[string]string{"id": created.ID, "status": string(created.Status)})
		},
	}
	// Without checkLimits, Prepare stays nil and the store saves the
	// notification as the caller last set it.
	if checkLimits != nil {
		cmd.Prepare = func(domain.Templates) (domain.NewNotification, error) {
			if err := checkLimits(); err != nil {
				return domain.NewNotification{}, err
			}
			return n, nil
		}
	}
	return cmd
}

type outcome struct {
	result domain.CreateResult
	err    error
}

func runCreate(ctx context.Context, store *postgres.Store, cmd domain.CreateCommand) <-chan outcome {
	ch := make(chan outcome, 1)
	go func() {
		r, err := store.CreateNotification(ctx, cmd)
		ch <- outcome{r, err}
	}()
	return ch
}

// waitForLockWait returns once a backend in this database is waiting on a
// lock, which here can only be B's key insert waiting for A's transaction.
func waitForLockWait(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for {
		var waiting int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		if err != nil {
			t.Fatalf("waiting for B to block on the key: %v", err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("B never blocked on A's key reservation")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func receive(ctx context.Context, t *testing.T, name string, ch <-chan outcome) outcome {
	t.Helper()
	select {
	case o := <-ch:
		return o
	case <-ctx.Done():
		t.Fatalf("%s did not finish", name)
		return outcome{}
	}
}

func count(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, query).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// TestConcurrentRepeatCommit: A reserves the key and holds its transaction
// open; B arrives with the same key and waits; A commits. B must find no row
// from its insert and return A's stored response (K10).
func TestConcurrentRepeatCommit(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	reserved, release := make(chan struct{}), make(chan struct{})
	a := runCreate(ctx, store, command("key-commit", func() error {
		close(reserved) // A owns the key; its transaction stays open
		<-release
		return nil
	}))
	select {
	case <-reserved:
	case <-ctx.Done():
		t.Fatal("A never reserved the key")
	}

	b := runCreate(ctx, store, command("key-commit", func() error {
		t.Error("B checked the limits, so it wrote a notification of its own")
		return nil
	}))
	waitForLockWait(ctx, t, pool)
	close(release)

	ra := receive(ctx, t, "A", a)
	rb := receive(ctx, t, "B", b)
	if ra.err != nil || rb.err != nil {
		t.Fatalf("A err=%v, B err=%v", ra.err, rb.err)
	}
	if ra.result.Replayed || !rb.result.Replayed {
		t.Fatalf("replayed: A=%v B=%v, want A=false B=true", ra.result.Replayed, rb.result.Replayed)
	}
	if !bytes.Equal(ra.result.Body, rb.result.Body) || rb.result.Status != 202 {
		t.Fatalf("B got %d %s, want A's 202 %s", rb.result.Status, rb.result.Body, ra.result.Body)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM notifications`); n != 1 {
		t.Fatalf("%d notifications, want 1", n)
	}
}

// TestConcurrentRepeatRollback: A reserves the key and holds; B waits; A rolls
// back (its content is over the limit). B's insert then succeeds and B writes
// the only notification (K10).
func TestConcurrentRepeatRollback(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	reserved, release := make(chan struct{}), make(chan struct{})
	a := runCreate(ctx, store, command("key-rollback", func() error {
		close(reserved)
		<-release
		return errLimit // A rolls back; the key stays free
	}))
	select {
	case <-reserved:
	case <-ctx.Done():
		t.Fatal("A never reserved the key")
	}

	bChecked := false
	b := runCreate(ctx, store, command("key-rollback", func() error {
		bChecked = true
		return nil
	}))
	waitForLockWait(ctx, t, pool)
	close(release)

	ra := receive(ctx, t, "A", a)
	rb := receive(ctx, t, "B", b)
	if !errors.Is(ra.err, errLimit) {
		t.Fatalf("A err = %v, want the limit error", ra.err)
	}
	if rb.err != nil || rb.result.Replayed || !bChecked {
		t.Fatalf("B: err=%v replayed=%v checked=%v; want a new notification", rb.err, rb.result.Replayed, bChecked)
	}
	if n := count(ctx, t, pool, `SELECT count(*) FROM notifications`); n != 1 {
		t.Fatalf("%d notifications, want 1", n)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT notification_id::text FROM idempotency_keys WHERE key = 'key-rollback'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != rb.result.NotificationID {
		t.Fatalf("key owned by %s, want B's notification %s", owner, rb.result.NotificationID)
	}
}

// TestGetNotificationRow reads back a saved notification and an unknown ID.
func TestGetNotificationRow(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	ctx := t.Context()

	created, err := store.CreateNotification(ctx, command("", nil))
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetNotification(ctx, created.NotificationID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.StatusQueued || got.Recipient != "+905551234567" || got.Channel != domain.ChannelSMS ||
		got.Priority != domain.PriorityNormal || got.CorrelationID != "test" || got.Attempts != 0 || got.LastError != nil {
		t.Fatalf("got %+v", got)
	}

	if _, err := store.GetNotification(ctx, "01900000-0000-7000-8000-000000000000"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrNotFound", err)
	}
}
