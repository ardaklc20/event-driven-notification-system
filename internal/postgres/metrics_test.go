package postgres_test

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// asOf is the fixed instant the metrics tests read at.
var asOf = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// metricRow is one notification placed for a metrics test. Zero times take
// asOf − 1 hour.
type metricRow struct {
	channel     domain.Channel
	priority    domain.Priority
	status      domain.Status
	availableAt time.Time
	createdAt   time.Time
	completedAt *time.Time
}

func insertMetricRows(t *testing.T, pool *pgxpool.Pool, rows ...metricRow) {
	t.Helper()
	for _, r := range rows {
		if r.priority == "" {
			r.priority = domain.PriorityNormal
		}
		if r.availableAt.IsZero() {
			r.availableAt = asOf.Add(-time.Hour)
		}
		if r.createdAt.IsZero() {
			r.createdAt = asOf.Add(-time.Hour)
		}
		var sentAt *time.Time
		if r.status == domain.StatusSent {
			sentAt = r.completedAt
		}
		if _, err := pool.Exec(t.Context(),
			`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status,
			                            available_at, created_at, sent_at, completed_at, lease_token, lease_expires_at)
			 VALUES ('r', $1, 'metrics', $2::text::notification_priority, 'corr', $3::text::notification_status,
			         $4, $5, $6, $7,
			         CASE WHEN $3 = 'processing' THEN gen_random_uuid() END,
			         CASE WHEN $3 = 'processing' THEN now() + interval '30 seconds' END)`,
			string(r.channel), string(r.priority), string(r.status), r.availableAt, r.createdAt, sentAt, r.completedAt); err != nil {
			t.Fatal(err)
		}
	}
}

func afterAsOf(d time.Duration) *time.Time {
	v := asOf.Add(d)
	return &v
}

func metricsAt(t *testing.T, pool *pgxpool.Pool) domain.Metrics {
	t.Helper()
	m, err := postgres.MetricsAt(t.Context(), pool, asOf)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestMetricsQueueDepth: ready (available_at at or before as_of) and deferred
// rows, in a known number per channel and priority, are counted exactly; rows
// in other statuses are not queue (R14, K20).
func TestMetricsQueueDepth(t *testing.T) {
	pool := testdb.New(t)
	want := map[domain.Channel]map[domain.Priority]domain.QueueDepth{}
	for ci, ch := range domain.Channels {
		want[ch] = map[domain.Priority]domain.QueueDepth{}
		for pi, p := range domain.Priorities {
			ready, deferred := ci*3+pi+1, (ci+pi)%3
			want[ch][p] = domain.QueueDepth{Ready: ready, Deferred: deferred}
			for i := range ready {
				avail := asOf.Add(-time.Duration(i+1) * time.Second)
				if i == 0 {
					avail = asOf // exactly at as_of counts as ready
				}
				insertMetricRows(t, pool, metricRow{channel: ch, priority: p, status: domain.StatusQueued, availableAt: avail})
			}
			for i := range deferred {
				insertMetricRows(t, pool, metricRow{channel: ch, priority: p, status: domain.StatusQueued,
					availableAt: asOf.Add(time.Duration(i)*time.Second + time.Microsecond)})
			}
			insertMetricRows(t, pool,
				metricRow{channel: ch, priority: p, status: domain.StatusProcessing},
				metricRow{channel: ch, priority: p, status: domain.StatusSent, completedAt: afterAsOf(-time.Second)},
				metricRow{channel: ch, priority: p, status: domain.StatusCancelled})
		}
	}
	m := metricsAt(t, pool)
	for _, ch := range domain.Channels {
		for _, p := range domain.Priorities {
			got := m.Channels[ch].Queue[p]
			if got.Ready != want[ch][p].Ready || got.Deferred != want[ch][p].Deferred {
				t.Errorf("%s/%s: ready %d deferred %d, want %d and %d", ch, p, got.Ready, got.Deferred,
					want[ch][p].Ready, want[ch][p].Deferred)
			}
		}
	}
}

// TestMetricsOldestReady: the age per priority is as_of − the earliest
// available_at of the ready rows; a priority with no ready row gives nil
// (K8, K20).
func TestMetricsOldestReady(t *testing.T) {
	pool := testdb.New(t)
	insertMetricRows(t, pool,
		metricRow{channel: domain.ChannelSMS, priority: domain.PriorityHigh, status: domain.StatusQueued, availableAt: asOf.Add(-10 * time.Second)},
		metricRow{channel: domain.ChannelSMS, priority: domain.PriorityHigh, status: domain.StatusQueued, availableAt: asOf.Add(-3 * time.Second)},
		metricRow{channel: domain.ChannelSMS, priority: domain.PriorityNormal, status: domain.StatusQueued, availableAt: asOf.Add(-2500 * time.Millisecond)},
		// Only deferred: no ready row, so no age.
		metricRow{channel: domain.ChannelSMS, priority: domain.PriorityLow, status: domain.StatusQueued, availableAt: asOf.Add(time.Minute)},
		// An older row that is not queued does not count.
		metricRow{channel: domain.ChannelSMS, priority: domain.PriorityNormal, status: domain.StatusProcessing, availableAt: asOf.Add(-time.Hour)},
	)
	m := metricsAt(t, pool)
	want := map[domain.Priority]*time.Duration{
		domain.PriorityHigh:   new(10 * time.Second),
		domain.PriorityNormal: new(2500 * time.Millisecond),
		domain.PriorityLow:    nil,
	}
	for p, w := range want {
		got := m.Channels[domain.ChannelSMS].Queue[p].OldestReadyAge
		if (got == nil) != (w == nil) || (got != nil && *got != *w) {
			t.Errorf("sms/%s: oldest ready age %v, want %v", p, deref(got), deref(w))
		}
	}
	for _, p := range domain.Priorities {
		if got := m.Channels[domain.ChannelEmail].Queue[p].OldestReadyAge; got != nil {
			t.Errorf("email/%s: oldest ready age %v, want nil", p, *got)
		}
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// TestMetricsProcessing: 3 processing on sms, 0 on email, 1 on push
// (K20, K30).
func TestMetricsProcessing(t *testing.T) {
	pool := testdb.New(t)
	for range 3 {
		insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusProcessing})
	}
	insertMetricRows(t, pool,
		metricRow{channel: domain.ChannelPush, status: domain.StatusProcessing},
		metricRow{channel: domain.ChannelEmail, status: domain.StatusQueued})
	m := metricsAt(t, pool)
	for ch, want := range map[domain.Channel]int{domain.ChannelSMS: 3, domain.ChannelEmail: 0, domain.ChannelPush: 1} {
		if got := m.Channels[ch].Processing; got != want {
			t.Errorf("%s processing = %d, want %d", ch, got, want)
		}
	}
}

// TestMetricsCompletions: 8 sent and 2 failed in the window give 8/60 and
// 2/60 per second and a ratio of 0.8; cancellations are not completions, so a
// channel with only cancellations reports 0 and 0 and no ratio (R14, K20).
func TestMetricsCompletions(t *testing.T) {
	t.Run("sent and failed", func(t *testing.T) {
		pool := testdb.New(t)
		for i := range 8 {
			insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusSent,
				completedAt: afterAsOf(-time.Duration(i+1) * time.Second)})
		}
		for i := range 2 {
			insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusFailed,
				completedAt: afterAsOf(-time.Duration(i+20) * time.Second)})
		}
		for range 4 {
			insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusCancelled})
		}
		c := metricsAt(t, pool).Channels[domain.ChannelSMS].Completed
		if c.Sent != 8 || c.Failed != 2 {
			t.Fatalf("sent %d failed %d, want 8 and 2", c.Sent, c.Failed)
		}
		if c.SentPerSecond() != 8.0/60 || c.FailedPerSecond() != 2.0/60 {
			t.Errorf("rates %v and %v, want 8/60 and 2/60", c.SentPerSecond(), c.FailedPerSecond())
		}
		if r := c.SuccessRatio(); r == nil || *r != 0.8 {
			t.Errorf("success ratio %v, want 0.8", deref(r))
		}
	})

	t.Run("only cancellations", func(t *testing.T) {
		pool := testdb.New(t)
		for range 4 {
			insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusCancelled})
		}
		c := metricsAt(t, pool).Channels[domain.ChannelSMS].Completed
		if c.Sent != 0 || c.Failed != 0 || c.SentPerSecond() != 0 || c.FailedPerSecond() != 0 || c.SuccessRatio() != nil {
			t.Errorf("completions %+v, rates %v and %v, ratio %v; want zeros and no ratio",
				c, c.SentPerSecond(), c.FailedPerSecond(), deref(c.SuccessRatio()))
		}
	})
}

// TestMetricsWindowEdges: the window is (as_of − 60 s, as_of]. Of rows
// completed at as_of − 60 s, as_of − 60 s + 1 µs, as_of and as_of + 1 µs, only
// the second and third count (K20).
func TestMetricsWindowEdges(t *testing.T) {
	pool := testdb.New(t)
	for _, d := range []time.Duration{-time.Minute, -time.Minute + time.Microsecond, 0, time.Microsecond} {
		insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusSent, completedAt: afterAsOf(d)})
	}
	if got := metricsAt(t, pool).Channels[domain.ChannelSMS].Completed.Sent; got != 2 {
		t.Errorf("sent in the window = %d, want 2 (as_of − 60 s + 1 µs and as_of)", got)
	}
}

// percentileCont is PostgreSQL's percentile_cont: linear interpolation
// between the two nearest sorted values.
func percentileCont(sorted []float64, p float64) float64 {
	pos := p * float64(len(sorted)-1)
	lo := int(math.Floor(pos))
	if lo+1 >= len(sorted) {
		return sorted[lo]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// TestMetricsLatency: 100 sent rows with known created_at → sent_at
// latencies give the same p50, p95 and p99 as computed here; a channel with
// no sent row gives 0 samples and no percentiles (R14, K20).
func TestMetricsLatency(t *testing.T) {
	pool := testdb.New(t)
	var latencies []float64
	for i := range 100 {
		lat := time.Duration((i*37)%100+1) * 1013 * time.Microsecond // shuffled, microsecond precision
		done := asOf.Add(-time.Duration(i+1) * 100 * time.Millisecond)
		insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusSent,
			createdAt: done.Add(-lat), completedAt: &done})
		latencies = append(latencies, float64(lat.Microseconds())/1000)
	}
	// A failed row in the window is not a latency sample.
	insertMetricRows(t, pool, metricRow{channel: domain.ChannelSMS, status: domain.StatusFailed, completedAt: afterAsOf(-time.Second)})
	slices.Sort(latencies)

	m := metricsAt(t, pool)
	l := m.Channels[domain.ChannelSMS].Latency
	if l.Samples != 100 {
		t.Fatalf("samples = %d, want 100", l.Samples)
	}
	for name, c := range map[string]struct {
		got  *float64
		want float64
	}{
		"p50": {l.P50, percentileCont(latencies, 0.5)},
		"p95": {l.P95, percentileCont(latencies, 0.95)},
		"p99": {l.P99, percentileCont(latencies, 0.99)},
	} {
		if c.got == nil || math.Abs(*c.got-c.want) > 1e-9 {
			t.Errorf("%s = %v ms, want %v", name, deref(c.got), c.want)
		}
	}
	t.Logf("p50 %.4f ms, p95 %.4f ms, p99 %.4f ms over 100 samples", *l.P50, *l.P95, *l.P99)
	if e := m.Channels[domain.ChannelEmail].Latency; e.Samples != 0 || e.P50 != nil || e.P95 != nil || e.P99 != nil {
		t.Errorf("email latency %+v, want 0 samples and no percentiles", e)
	}
}

func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(t.Context(), `SELECT clock_timestamp()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// TestMetricsAsOfFirstRead: as_of is the database clock read inside the
// call: after a database time read before it and before one read after it.
func TestMetricsAsOfFirstRead(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	before := dbNow(t, pool)
	m, err := store.Metrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	after := dbNow(t, pool)
	if m.AsOf.Before(before) || m.AsOf.After(after) {
		t.Errorf("as_of %s is not between %s and %s", m.AsOf.Format(time.RFC3339Nano),
			before.Format(time.RFC3339Nano), after.Format(time.RFC3339Nano))
	}
	if len(m.Channels) != 3 {
		t.Errorf("%d channels, want 3", len(m.Channels))
	}
}

// TestMetricsSingleSnapshot: right after the first statement read as_of,
// another connection marks the one ready sms row sent and commits. Every
// later query of the call still sees the state before the change: ready 1
// and sent 0. Read outside one snapshot, the queue would show 0 ready and the
// window 1 sent (K20).
func TestMetricsSingleSnapshot(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	other := secondPoolFor(t, pool)
	var id string
	if err := pool.QueryRow(t.Context(),
		`INSERT INTO notifications (recipient, channel, content, priority, correlation_id, available_at)
		 VALUES ('+905551234567', 'sms', 'snapshot', 'normal', 'corr', clock_timestamp() - interval '1 second')
		 RETURNING id::text`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	postgres.SetAfterAsOf(store, func(asOf time.Time) {
		tag, err := other.Exec(t.Context(),
			`UPDATE notifications SET status = 'sent', sent_at = $2, completed_at = $2, updated_at = $2
			 WHERE id = $1::text::uuid`, id, asOf.Add(-time.Second))
		if err != nil || tag.RowsAffected() != 1 {
			t.Errorf("concurrent change: rows=%d err=%v", tag.RowsAffected(), err)
		}
	})
	m, err := store.Metrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sms := m.Channels[domain.ChannelSMS]
	if r, s := sms.Queue[domain.PriorityNormal].Ready, sms.Completed.Sent; r != 1 || s != 0 {
		t.Errorf("ready %d sent %d, want 1 and 0: the state at as_of", r, s)
	}

	postgres.SetAfterAsOf(store, nil)
	m, err = store.Metrics(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sms = m.Channels[domain.ChannelSMS]
	if r, s := sms.Queue[domain.PriorityNormal].Ready, sms.Completed.Sent; r != 0 || s != 1 {
		t.Errorf("next call: ready %d sent %d, want 0 and 1: the change was committed", r, s)
	}
}

// TestMetricsExplainAnalyze: with 100,000 rows across statuses, each metrics
// query is run under EXPLAIN ANALYZE and its plan and time logged; then the
// whole call is timed (K20).
func TestMetricsExplainAnalyze(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status,
		                           available_at, created_at, sent_at, completed_at, lease_token, lease_expires_at)
		SELECT 'r', (ARRAY['sms','email','push'])[1 + g % 3], 'c',
		       (ARRAY['high','normal','low'])[1 + (g / 3) % 3]::notification_priority, 'corr', s.status,
		       clock_timestamp() + (g % 600 - 300) * interval '1 second',
		       clock_timestamp() - (g % 3600) * interval '1 second' - interval '1 second',
		       CASE WHEN s.status = 'sent' THEN clock_timestamp() - (g % 3600) * interval '1 second' END,
		       CASE WHEN s.status IN ('sent', 'failed') THEN clock_timestamp() - (g % 3600) * interval '1 second' END,
		       CASE WHEN s.status = 'processing' THEN gen_random_uuid() END,
		       CASE WHEN s.status = 'processing' THEN clock_timestamp() + interval '30 seconds' END
		FROM generate_series(1, 100000) AS g,
		     LATERAL (SELECT (CASE WHEN g % 10 < 4 THEN 'queued' WHEN g % 10 = 4 THEN 'processing'
		                           WHEN g % 10 < 9 THEN 'sent' WHEN g % 20 = 9 THEN 'failed'
		                           ELSE 'cancelled' END)::notification_status AS status) AS s`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `VACUUM ANALYZE notifications`); err != nil {
		t.Fatal(err)
	}
	var counts string
	if err := pool.QueryRow(t.Context(),
		`SELECT string_agg(status::text || '=' || n, ' ' ORDER BY status) FROM
		 (SELECT status, count(*) AS n FROM notifications GROUP BY status) c`).Scan(&counts); err != nil {
		t.Fatal(err)
	}
	t.Logf("rows by status: %s", counts)

	now := dbNow(t, pool)
	for _, q := range []struct {
		name  string
		query string
		args  []any
	}{
		{"queue", postgres.MetricsQueueSQL, []any{now}},
		{"processing", postgres.MetricsProcessingSQL, nil},
		{"completed", postgres.MetricsCompletedSQL, []any{now, domain.MetricsWindow.Microseconds()}},
	} {
		rows, err := pool.Query(t.Context(), "EXPLAIN (ANALYZE, BUFFERS) "+q.query, q.args...)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s:\n%s", q.name, strings.Join(lines, "\n"))
	}

	for i := range 3 {
		start := time.Now()
		if _, err := store.Metrics(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Logf("Metrics call %d: %s", i+1, time.Since(start).Round(10*time.Microsecond))
	}
}
