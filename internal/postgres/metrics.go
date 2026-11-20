package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// The metrics queries take as_of as $1 and never read the clock themselves,
// so every value refers to the same instant and tests can fix it (K20).
const (
	metricsAsOfSQL = `SELECT clock_timestamp()`

	metricsQueueSQL = `
		SELECT channel, priority::text,
		       count(*) FILTER (WHERE available_at <= $1),
		       count(*) FILTER (WHERE available_at > $1),
		       min(available_at) FILTER (WHERE available_at <= $1)
		FROM notifications
		WHERE status = 'queued'
		GROUP BY channel, priority`

	metricsProcessingSQL = `
		SELECT channel, count(*)
		FROM notifications
		WHERE status = 'processing'
		GROUP BY channel`

	// The window is half-open: after as_of − window, at or before as_of.
	// Latency is split (K26): created_at → sent_at for the notifications sent
	// without scheduled_at, scheduled_at → sent_at for the scheduled ones.
	metricsCompletedSQL = `
		SELECT channel,
		       count(*) FILTER (WHERE status = 'sent'),
		       count(*) FILTER (WHERE status = 'failed'),
		       count(*) FILTER (WHERE status = 'sent' AND scheduled_at IS NULL),
		       percentile_cont(ARRAY[0.5, 0.95, 0.99]::float8[]) WITHIN GROUP (
		           ORDER BY (extract(epoch FROM sent_at - created_at) * 1000)::float8
		       ) FILTER (WHERE status = 'sent' AND scheduled_at IS NULL),
		       count(*) FILTER (WHERE status = 'sent' AND scheduled_at IS NOT NULL),
		       percentile_cont(ARRAY[0.5, 0.95, 0.99]::float8[]) WITHIN GROUP (
		           ORDER BY (extract(epoch FROM sent_at - scheduled_at) * 1000)::float8
		       ) FILTER (WHERE status = 'sent' AND scheduled_at IS NOT NULL)
		FROM notifications
		WHERE completed_at > $1::timestamptz - $2::bigint * interval '1 microsecond'
		  AND completed_at <= $1::timestamptz
		GROUP BY channel`
)

// Metrics reads one snapshot (K20): a REPEATABLE READ READ ONLY transaction
// whose first statement reads clock_timestamp() as as_of and takes the
// snapshot; every later query gets as_of as a parameter.
func (s *Store) Metrics(ctx context.Context) (domain.Metrics, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Metrics{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var asOf time.Time
	if err := tx.QueryRow(ctx, metricsAsOfSQL).Scan(&asOf); err != nil {
		return domain.Metrics{}, fmt.Errorf("metrics as_of: %w", err)
	}
	if s.afterAsOf != nil {
		s.afterAsOf(asOf)
	}
	m, err := metricsAt(ctx, tx, asOf)
	if err != nil {
		return domain.Metrics{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Metrics{}, fmt.Errorf("commit: %w", err)
	}
	return m, nil
}

// metricsAt runs the metrics queries on db for asOf. Every channel and
// priority is present, with zeros where no row matched.
func metricsAt(ctx context.Context, db querier, asOf time.Time) (domain.Metrics, error) {
	channels := make(map[domain.Channel]*domain.ChannelMetrics, len(domain.Channels))
	for _, ch := range domain.Channels {
		cm := &domain.ChannelMetrics{Queue: make(map[domain.Priority]domain.QueueDepth, len(domain.Priorities))}
		for _, p := range domain.Priorities {
			cm.Queue[p] = domain.QueueDepth{}
		}
		channels[ch] = cm
	}

	var (
		channel, priority string
		ready, deferred   int
		oldest            *time.Time
	)
	if err := forEach(ctx, db, metricsQueueSQL, []any{asOf}, []any{&channel, &priority, &ready, &deferred, &oldest}, func() error {
		q := domain.QueueDepth{Ready: ready, Deferred: deferred}
		if oldest != nil {
			age := asOf.Sub(*oldest)
			q.OldestReadyAge = &age
		}
		channels[domain.Channel(channel)].Queue[domain.Priority(priority)] = q
		return nil
	}); err != nil {
		return domain.Metrics{}, fmt.Errorf("metrics queue: %w", err)
	}

	var processing int
	if err := forEach(ctx, db, metricsProcessingSQL, nil, []any{&channel, &processing}, func() error {
		channels[domain.Channel(channel)].Processing = processing
		return nil
	}); err != nil {
		return domain.Metrics{}, fmt.Errorf("metrics processing: %w", err)
	}

	var (
		sent, failed, immediate, scheduled int
		immediateP, scheduledP             []float64
	)
	if err := forEach(ctx, db, metricsCompletedSQL, []any{asOf, domain.MetricsWindow.Microseconds()},
		[]any{&channel, &sent, &failed, &immediate, &immediateP, &scheduled, &scheduledP}, func() error {
			cm := channels[domain.Channel(channel)]
			cm.Completed = domain.Completions{Sent: sent, Failed: failed}
			cm.Latency = latency(immediate, immediateP)
			cm.ScheduledLatency = latency(scheduled, scheduledP)
			return nil
		}); err != nil {
		return domain.Metrics{}, fmt.Errorf("metrics completed: %w", err)
	}

	m := domain.Metrics{AsOf: asOf, Channels: make(map[domain.Channel]domain.ChannelMetrics, len(channels))}
	for ch, cm := range channels {
		m.Channels[ch] = *cm
	}
	return m, nil
}

// forEach runs query on db and calls fn after scanning each row into dest.
func forEach(ctx context.Context, db querier, query string, args, dest []any, fn func() error) error {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	_, err = pgx.ForEachRow(rows, dest, fn)
	return err
}

// latency builds a Latency from its sample count and the three percentiles,
// which are null without samples.
func latency(samples int, p []float64) domain.Latency {
	l := domain.Latency{Samples: samples}
	if len(p) == 3 {
		p50, p95, p99 := p[0], p[1], p[2]
		l.P50, l.P95, l.P99 = &p50, &p95, &p99
	}
	return l
}
