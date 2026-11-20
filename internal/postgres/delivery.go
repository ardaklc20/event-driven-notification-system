package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

const (
	// claimSQL takes the oldest due queued row that still has attempts left
	// and that no other transaction has locked, puts it under a new lease and
	// counts the attempt, in one statement (K2, K3, K9). The remaining lease
	// comes back as microseconds computed by the database clock, so the worker
	// never adds its own idea of the claim's round trip to it.
	claimSQL = `
		WITH next AS (
			SELECT id
			FROM notifications
			WHERE status = 'queued' AND available_at <= clock_timestamp() AND attempts < $2
			ORDER BY available_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notifications AS n
		SET status           = 'processing',
		    lease_token      = gen_random_uuid(),
		    lease_expires_at = clock_timestamp() + $1::bigint * interval '1 microsecond',
		    attempts         = n.attempts + 1,
		    updated_at       = clock_timestamp()
		FROM next
		WHERE n.id = next.id
		RETURNING n.id::text, n.recipient, n.channel, n.content, n.correlation_id, n.attempts,
		          n.lease_token::text,
		          floor(extract(epoch FROM n.lease_expires_at - clock_timestamp()) * 1000000)::bigint`

	// Every result write reads the database clock once (c.now) and carries the
	// lease condition (K3, K9): the row is still processing, under this claim's
	// token, and the lease has not expired by the database clock. Each write
	// also clears both lease columns.
	resultFrom = `
		FROM (SELECT clock_timestamp() AS now) AS c
		WHERE n.id = $1::text::uuid
		  AND n.status = 'processing'
		  AND n.lease_token = $2::text::uuid
		  AND n.lease_expires_at > c.now`

	markSentSQL = `
		UPDATE notifications AS n
		SET status              = 'sent',
		    sent_at             = c.now,
		    provider_message_id = $3,
		    provider_timestamp  = $4,
		    lease_token         = NULL,
		    lease_expires_at    = NULL,
		    updated_at          = c.now` + resultFrom

	markFailedSQL = `
		UPDATE notifications AS n
		SET status           = 'failed',
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = c.now` + resultFrom

	// markRetrySQL puts a temporary failure back in the queue; the wait is
	// added to the database clock, so a waiting retry holds no worker and
	// survives a crash (K7).
	markRetrySQL = `
		UPDATE notifications AS n
		SET status           = 'queued',
		    available_at     = c.now + $4::bigint * interval '1 microsecond',
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = c.now` + resultFrom

	// recoverSQL takes back at most $2 rows whose lease has expired by the
	// database clock (K3, K9, K40). SKIP LOCKED lets several workers recover at
	// the same time without taking the same row. A row with attempts left goes
	// back to queued; one without fails. It changes statuses only: no permit,
	// no circuit breaker.
	recoverSQL = `
		WITH expired AS (
			SELECT id
			FROM notifications
			WHERE status = 'processing' AND lease_expires_at <= clock_timestamp()
			ORDER BY lease_expires_at, id
			LIMIT $2
			FOR UPDATE SKIP LOCKED
		)
		UPDATE notifications AS n
		SET status           = CASE WHEN n.attempts < $1 THEN 'queued' ELSE 'failed' END::notification_status,
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = clock_timestamp()
		FROM expired
		WHERE n.id = expired.id
		RETURNING n.id::text, n.correlation_id, n.status::text, n.attempts`
)

// RecoveredError is the last_error recovery writes.
const RecoveredError = "temporary: lease expired before a result was written"

// ClaimNext claims one due queued notification that has had fewer than
// maxAttempts attempts. ok is false when there is none.
func (s *Store) ClaimNext(ctx context.Context, lease time.Duration, maxAttempts int) (claim domain.Claim, ok bool, err error) {
	var (
		channel     string
		remainingUS int64
	)
	err = s.pool.QueryRow(ctx, claimSQL, lease.Microseconds(), maxAttempts).Scan(
		&claim.ID, &claim.Recipient, &channel, &claim.Content, &claim.CorrelationID, &claim.Attempt,
		&claim.LeaseToken, &remainingUS)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Claim{}, false, nil
	}
	if err != nil {
		return domain.Claim{}, false, fmt.Errorf("claim: %w", err)
	}
	claim.Channel = domain.Channel(channel)
	claim.LeaseRemaining = time.Duration(remainingUS) * time.Microsecond
	return claim, true, nil
}

// MarkSent records the provider's acceptance. It reports false when the lease
// was no longer valid, in which case no row changed.
func (s *Store) MarkSent(ctx context.Context, id, leaseToken string, acc domain.Acceptance) (bool, error) {
	var messageID *string
	if acc.MessageID != "" {
		messageID = &acc.MessageID
	}
	var timestamp *time.Time
	if !acc.Timestamp.IsZero() {
		timestamp = &acc.Timestamp
	}
	return s.exec(ctx, "mark sent", markSentSQL, id, leaseToken, messageID, timestamp)
}

// MarkFailed ends the notification as failed with lastError. It reports false
// when the lease was no longer valid, in which case no row changed.
func (s *Store) MarkFailed(ctx context.Context, id, leaseToken, lastError string) (bool, error) {
	return s.exec(ctx, "mark failed", markFailedSQL, id, leaseToken, lastError)
}

// MarkRetry puts the notification back to queued, due wait after the database
// clock, with lastError. It reports false when the lease was no longer valid,
// in which case no row changed.
func (s *Store) MarkRetry(ctx context.Context, id, leaseToken, lastError string, wait time.Duration) (bool, error) {
	return s.exec(ctx, "mark retry", markRetrySQL, id, leaseToken, lastError, wait.Microseconds())
}

func (s *Store) exec(ctx context.Context, what, query string, args ...any) (bool, error) {
	tag, err := s.pool.Exec(ctx, query, args...)
	if err != nil {
		return false, fmt.Errorf("%s: %w", what, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecoverExpired takes back at most batch notifications whose lease expired:
// queued while they have had fewer than maxAttempts attempts, failed otherwise.
func (s *Store) RecoverExpired(ctx context.Context, maxAttempts, batch int) ([]domain.Recovered, error) {
	rows, err := s.pool.Query(ctx, recoverSQL, maxAttempts, batch, RecoveredError)
	if err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Recovered, error) {
		var (
			r      domain.Recovered
			status string
		)
		err := row.Scan(&r.ID, &r.CorrelationID, &status, &r.Attempts)
		r.Status = domain.Status(status)
		return r, err
	})
	if err != nil {
		return nil, fmt.Errorf("recover: %w", err)
	}
	return out, nil
}
