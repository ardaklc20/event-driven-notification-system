package postgres_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// dbAt returns the database clock plus d as an RFC 3339 time in UTC.
func dbAt(t *testing.T, pool *pgxpool.Pool, d time.Duration) string {
	t.Helper()
	return dbNow(t, pool).Add(d).UTC().Format(time.RFC3339Nano)
}

// waitDBPast waits until the database clock has passed the RFC 3339 time s.
func waitDBPast(t *testing.T, pool *pgxpool.Pool, s string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var past bool
		if err := pool.QueryRow(t.Context(), `SELECT $1::timestamptz < clock_timestamp()`, s).Scan(&past); err != nil {
			t.Fatal(err)
		}
		if past {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("database clock never passed %s", s)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestScheduledNotClaimedEarly: a notification scheduled 3 seconds ahead is
// not claimed before its time and is claimed after it (B2, K26).
func TestScheduledNotClaimedEarly(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	cmd := command("", nil)
	cmd.Notification.ScheduledAt = dbAt(t, pool, 3*time.Second)
	res, err := store.CreateNotification(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	var created struct{ ID string }
	if err := json.Unmarshal(res.Body, &created); err != nil {
		t.Fatal(err)
	}
	var sameTimes bool
	if err := pool.QueryRow(t.Context(),
		`SELECT scheduled_at = $2::timestamptz AND available_at = scheduled_at FROM notifications WHERE id = $1::uuid`,
		created.ID, cmd.Notification.ScheduledAt).Scan(&sameTimes); err != nil || !sameTimes {
		t.Fatalf("scheduled_at and available_at equal the request's time: %v err=%v", sameTimes, err)
	}

	if c, ok, err := claim(t.Context(), store, request(domain.ChannelSMS)); err != nil || ok {
		t.Fatalf("claim before the time: claimed %+v ok=%v err=%v, want nothing", c, ok, err)
	}
	waitDBPast(t, pool, cmd.Notification.ScheduledAt)
	c, ok, err := claim(t.Context(), store, request(domain.ChannelSMS))
	if err != nil || !ok || c.ID != created.ID {
		t.Fatalf("claim after the time: %+v ok=%v err=%v, want %s", c, ok, err, created.ID)
	}
}

// TestScheduledBatchOrder: one batch holds three sms normal items scheduled
// at t+3, t+1 and t+2 seconds; they are claimed in the order t+1, t+2, t+3
// (K8, K26).
func TestScheduledBatchOrder(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	cmd := batchCommand("", "t+3", "t+1", "t+2")
	for i, d := range []time.Duration{3 * time.Second, time.Second, 2 * time.Second} {
		cmd.Items[i].ScheduledAt = dbAt(t, pool, d)
	}
	cmd.RequestHash = domain.RequestHash(domain.OperationBatch, cmd.Items)
	if _, err := store.CreateBatch(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	waitDBPast(t, pool, cmd.Items[0].ScheduledAt)

	var got []string
	for range 3 {
		c, ok, err := claim(t.Context(), store, request(domain.ChannelSMS))
		if err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		got = append(got, c.Content)
	}
	if want := []string{"t+1", "t+2", "t+3"}; !slicesEqual(got, want) {
		t.Fatalf("claim order %v, want %v", got, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestMetricsLatencySplit: immediate notifications are measured from
// created_at, scheduled ones from scheduled_at, each with its own sample
// count, so an hour of planned wait never shows in the immediate latency
// (K20, K26).
func TestMetricsLatencySplit(t *testing.T) {
	pool := testdb.New(t)
	insert := func(created, scheduled *time.Time, sent time.Time) {
		t.Helper()
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status,
			                            created_at, scheduled_at, available_at, sent_at, completed_at)
			 VALUES ('r', 'sms', 'split', 'normal', 'corr', 'sent', $1::timestamptz, $2::timestamptz,
			         coalesce($2::timestamptz, $1::timestamptz), $3::timestamptz, $3::timestamptz)`,
			*created, scheduled, sent); err != nil {
			t.Fatal(err)
		}
	}
	sent := asOf.Add(-10 * time.Second)
	for _, ms := range []int{100, 200, 300} { // immediate: created → sent
		created := sent.Add(-time.Duration(ms) * time.Millisecond)
		insert(&created, nil, sent)
	}
	for _, ms := range []int{50, 150} { // scheduled an hour after creation
		scheduled := sent.Add(-time.Duration(ms) * time.Millisecond)
		created := scheduled.Add(-time.Hour)
		insert(&created, &scheduled, sent)
	}

	m := metricsAt(t, pool).Channels[domain.ChannelSMS]
	if m.Completed.Sent != 5 {
		t.Fatalf("sent %d, want 5", m.Completed.Sent)
	}
	check := func(name string, l domain.Latency, samples int, p50, p99 float64) {
		t.Helper()
		if l.Samples != samples || l.P50 == nil || l.P99 == nil || *l.P50 != p50 || *l.P99 > p99 {
			t.Errorf("%s: samples %d p50 %v p99 %v, want %d samples, p50 %v, p99 at most %v",
				name, l.Samples, deref(l.P50), deref(l.P99), samples, p50, p99)
		}
	}
	check("immediate", m.Latency, 3, 200, 300)
	check("scheduled", m.ScheduledLatency, 2, 100, 150)
	t.Logf("immediate p50 %.0f ms p99 %.1f ms (3 samples); scheduled p50 %.0f ms p99 %.1f ms (2 samples)",
		*m.Latency.P50, *m.Latency.P99, *m.ScheduledLatency.P50, *m.ScheduledLatency.P99)
}
