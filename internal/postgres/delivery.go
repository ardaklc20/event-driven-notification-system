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
	// claimSQL takes the oldest due queued row that no other transaction has
	// locked, puts it under a new lease and counts the attempt, in one
	// statement (K2, K3, K9). The remaining lease comes back as microseconds
	// computed by the database clock, so the worker never adds its own idea
	// of the claim's round trip to it.
	claimSQL = `
		WITH next AS (
			SELECT id
			FROM notifications
			WHERE status = 'queued' AND available_at <= clock_timestamp()
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

	// The lease condition every result write carries (K3, K9): the row is
	// still processing, under this claim's token, and the lease has not
	// expired by the database clock. Each write also clears both lease columns.
	leaseCondition = `
		WHERE id = $1::text::uuid
		  AND status = 'processing'
		  AND lease_token = $2::text::uuid
		  AND lease_expires_at > clock_timestamp()`

	markSentSQL = `
		UPDATE notifications
		SET status              = 'sent',
		    sent_at             = clock_timestamp(),
		    provider_message_id = $3,
		    provider_timestamp  = $4,
		    lease_token         = NULL,
		    lease_expires_at    = NULL,
		    updated_at          = clock_timestamp()` + leaseCondition

	markFailedSQL = `
		UPDATE notifications
		SET status           = 'failed',
		    last_error       = $3,
		    lease_token      = NULL,
		    lease_expires_at = NULL,
		    updated_at       = clock_timestamp()` + leaseCondition
)

// ClaimNext claims one due queued notification for lease. ok is false when
// there is none.
func (s *Store) ClaimNext(ctx context.Context, lease time.Duration) (claim domain.Claim, ok bool, err error) {
	var (
		channel     string
		remainingUS int64
	)
	err = s.pool.QueryRow(ctx, claimSQL, lease.Microseconds()).Scan(
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
	tag, err := s.pool.Exec(ctx, markSentSQL, id, leaseToken, messageID, timestamp)
	if err != nil {
		return false, fmt.Errorf("mark sent: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// MarkFailed ends the notification as failed with lastError. It reports false
// when the lease was no longer valid, in which case no row changed.
func (s *Store) MarkFailed(ctx context.Context, id, leaseToken, lastError string) (bool, error) {
	tag, err := s.pool.Exec(ctx, markFailedSQL, id, leaseToken, lastError)
	if err != nil {
		return false, fmt.Errorf("mark failed: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}
