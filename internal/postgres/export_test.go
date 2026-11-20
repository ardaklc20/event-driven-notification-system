package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// ClaimSQL is the regular claim statement, for tests that inspect its plan or
// run it on a transaction they hold. ProbeClaimSQL is the probe's.
var (
	ClaimSQL      = claimSQL
	ProbeClaimSQL = probeClaimSQL
)

// MarkRetrySQL is the processing → queued result write, for race tests that
// run it on a transaction they hold. Its arguments: id, lease token, last
// error, wait in microseconds.
const MarkRetrySQL = markRetrySQL

// MarkSent, MarkFailed and MarkRetry are the result writes without the
// breaker, for the tests of K3's lease rules.
func (s *Store) MarkSent(ctx context.Context, id, leaseToken string, acc domain.Acceptance) (bool, error) {
	return writeResult(ctx, s.pool, domain.Attempt{ID: id, LeaseToken: leaseToken},
		domain.Result{Kind: domain.ResultSent, Acceptance: acc})
}

func (s *Store) MarkFailed(ctx context.Context, id, leaseToken, lastError string) (bool, error) {
	return writeResult(ctx, s.pool, domain.Attempt{ID: id, LeaseToken: leaseToken},
		domain.Result{Kind: domain.ResultFailed, LastError: lastError})
}

func (s *Store) MarkRetry(ctx context.Context, id, leaseToken, lastError string, wait time.Duration) (bool, error) {
	return writeResult(ctx, s.pool, domain.Attempt{ID: id, LeaseToken: leaseToken},
		domain.Result{Kind: domain.ResultRetry, LastError: lastError, Wait: wait})
}

// CancelIn runs the cancel's decision on tx without committing it.
func CancelIn(ctx context.Context, tx pgx.Tx, id string) (domain.Notification, error) {
	return cancelIn(ctx, tx, id)
}

// ListQuery is the listing query for f, for the test that inspects its plan.
func ListQuery(f domain.ListFilter) (string, []any) {
	return listQuery(f)
}

// Metrics queries, for plans and measurements.
const (
	MetricsQueueSQL      = metricsQueueSQL
	MetricsProcessingSQL = metricsProcessingSQL
	MetricsCompletedSQL  = metricsCompletedSQL
)

// MetricsAt runs the metrics queries for a fixed as_of, outside a snapshot.
func MetricsAt(ctx context.Context, pool *pgxpool.Pool, asOf time.Time) (domain.Metrics, error) {
	return metricsAt(ctx, pool, asOf)
}

// SetAfterAsOf sets the hook that runs right after the metrics snapshot read
// its as_of.
func SetAfterAsOf(s *Store, fn func(time.Time)) {
	s.afterAsOf = fn
}
