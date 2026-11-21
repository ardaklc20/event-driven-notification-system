package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// Store runs the notification queries (K17).
type Store struct {
	pool *pgxpool.Pool
	// afterAsOf, when set by a test, runs right after the metrics snapshot
	// read its as_of.
	afterAsOf func(time.Time)
}

// NewStore returns a Store on pool.
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const notificationColumns = `id::text, recipient, channel, content, priority::text, status::text,
	attempts, last_error, correlation_id, batch_id::text, provider_message_id, provider_timestamp, sent_at,
	created_at, updated_at, scheduled_at, template_id::text`

const (
	reserveKeySQL = `
		INSERT INTO idempotency_keys (key, operation, request_hash)
		VALUES ($1, $2, $3)
		ON CONFLICT (key) DO NOTHING
		RETURNING key`

	// A scheduled notification is first available at its scheduled time
	// (K26); any other at once, as the column default would make it.
	insertNotificationSQL = `
		INSERT INTO notifications (recipient, channel, content, priority, correlation_id, scheduled_at, available_at, template_id, traceparent)
		VALUES ($1, $2, $3, $4::text::notification_priority, $5, $6::timestamptz, coalesce($6::timestamptz, now()), $7::text::uuid,
		        nullif($8, ''))
		RETURNING ` + notificationColumns

	// pastScheduledSQL returns the zero-based positions of the times ($1, in
	// request order, null for an unscheduled item) that are at or before the
	// database clock (K26).
	pastScheduledSQL = `
		SELECT coalesce(array_agg(u.i - 1 ORDER BY u.i), '{}')::int[]
		FROM unnest($1::text[]::timestamptz[]) WITH ORDINALITY AS u(at, i)
		WHERE u.at <= clock_timestamp()`

	completeKeySQL = `
		UPDATE idempotency_keys
		SET response_status = $2, response_body = $3, notification_id = $4::text::uuid
		WHERE key = $1`

	readKeySQL = `
		SELECT operation, request_hash, response_status, response_body, notification_id::text
		FROM idempotency_keys
		WHERE key = $1`

	getNotificationSQL = `
		SELECT ` + notificationColumns + `
		FROM notifications
		WHERE id = $1::text::uuid`
)

// CreateNotification saves one notification under the K10 rules:
//
//  1. With a key, reserve it first with INSERT … ON CONFLICT DO NOTHING. If
//     another transaction holds the key, PostgreSQL makes this insert wait
//     until that transaction ends.
//  2. If no row comes back, the key was completed by a committed request:
//     roll back and read the stored response with a separate SELECT.
//  3. Otherwise (or without a key) load the template the request names, let
//     Prepare render it and check the content limits (K33), insert the
//     notification, store the response bytes with the key and commit.
func (s *Store) CreateNotification(ctx context.Context, cmd domain.CreateCommand) (domain.CreateResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if cmd.Key != "" {
		reserved, err := reserveKey(ctx, tx, cmd.Key, cmd.Operation, cmd.RequestHash)
		if err != nil {
			return domain.CreateResult{}, err
		}
		if !reserved {
			if err := tx.Rollback(ctx); err != nil {
				return domain.CreateResult{}, fmt.Errorf("rollback: %w", err)
			}
			return s.replay(ctx, cmd.Key, cmd.Operation, cmd.RequestHash)
		}
	}

	n := cmd.Notification
	if cmd.Prepare != nil {
		templates, err := loadTemplates(ctx, tx, []domain.NewNotification{n})
		if err != nil {
			return domain.CreateResult{}, err
		}
		if n, err = cmd.Prepare(templates); err != nil {
			return domain.CreateResult{}, err
		}
	}
	if err := checkScheduled(ctx, tx, []domain.NewNotification{n}, func(int) string { return "scheduled_at" }); err != nil {
		return domain.CreateResult{}, err
	}
	created, err := scanNotification(tx.QueryRow(ctx, insertNotificationSQL,
		n.Recipient, string(n.Channel), n.Content, string(n.Priority), cmd.CorrelationID, scheduledArg(n), templateArg(n), cmd.Traceparent))
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("insert notification: %w", err)
	}

	body, err := cmd.Respond(created)
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("build response: %w", err)
	}

	if cmd.Key != "" {
		if _, err := tx.Exec(ctx, completeKeySQL, cmd.Key, http.StatusAccepted, body, created.ID); err != nil {
			return domain.CreateResult{}, fmt.Errorf("store response: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return domain.CreateResult{}, fmt.Errorf("commit: %w", err)
	}
	return domain.CreateResult{NotificationID: created.ID, Status: http.StatusAccepted, Body: body}, nil
}

// checkScheduled applies K26's past-time rule to the items about to be
// saved: a scheduled_at at or before the database clock gives a validation
// error, which rolls the transaction back and leaves the key free. It runs
// only for a new key, so a repeat after the scheduled time still replays.
func checkScheduled(ctx context.Context, tx pgx.Tx, items []domain.NewNotification, field func(int) string) error {
	times := make([]*string, len(items))
	scheduled := false
	for i := range items {
		if items[i].ScheduledAt != "" {
			times[i] = &items[i].ScheduledAt
			scheduled = true
		}
	}
	if !scheduled {
		return nil
	}
	var past []int32
	if err := tx.QueryRow(ctx, pastScheduledSQL, times).Scan(&past); err != nil {
		return fmt.Errorf("check scheduled_at: %w", err)
	}
	if len(past) == 0 {
		return nil
	}
	errs := make([]domain.FieldError, len(past))
	for i, pos := range past {
		errs[i] = domain.PastError(field(int(pos)))
	}
	return &domain.ValidationError{Errors: errs}
}

// scheduledArg is the scheduled_at parameter: nil for an unscheduled
// notification.
func scheduledArg(n domain.NewNotification) *string {
	if n.ScheduledAt == "" {
		return nil
	}
	return &n.ScheduledAt
}

// reserveKey inserts the key row as the reservation. false means a committed
// request already completed the key; if another transaction holds it,
// PostgreSQL makes this insert wait until that transaction ends (K10).
func reserveKey(ctx context.Context, tx pgx.Tx, key string, op domain.Operation, hash []byte) (bool, error) {
	var reserved string
	err := tx.QueryRow(ctx, reserveKeySQL, key, string(op), hash).Scan(&reserved)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reserve key: %w", err)
	}
	return true, nil
}

// replay reads the response stored with a completed key. The SELECT is a new
// statement, so under Read Committed it sees the committed row.
func (s *Store) replay(ctx context.Context, key string, op domain.Operation, requestHash []byte) (domain.CreateResult, error) {
	var (
		operation      string
		hash           []byte
		status         *int16
		body           []byte
		notificationID *string
	)
	err := s.pool.QueryRow(ctx, readKeySQL, key).Scan(&operation, &hash, &status, &body, &notificationID)
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("read stored response: %w", err)
	}
	if operation != string(op) || !bytes.Equal(hash, requestHash) {
		return domain.CreateResult{}, domain.ErrIdempotencyConflict
	}
	if status == nil {
		// A committed key row is always complete; this would be a bug.
		return domain.CreateResult{}, errors.New("stored key has no response")
	}
	result := domain.CreateResult{Status: int(*status), Body: body, Replayed: true}
	if notificationID != nil {
		result.NotificationID = *notificationID
	}
	return result, nil
}

// GetNotification reads one notification by ID.
func (s *Store) GetNotification(ctx context.Context, id string) (domain.Notification, error) {
	n, err := scanNotification(s.pool.QueryRow(ctx, getNotificationSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("get notification: %w", err)
	}
	return n, nil
}

func scanNotification(row pgx.Row) (domain.Notification, error) {
	var (
		n        domain.Notification
		channel  string
		priority string
		status   string
	)
	err := row.Scan(&n.ID, &n.Recipient, &channel, &n.Content, &priority, &status,
		&n.Attempts, &n.LastError, &n.CorrelationID, &n.BatchID, &n.ProviderMessageID, &n.ProviderTimestamp, &n.SentAt,
		&n.CreatedAt, &n.UpdatedAt, &n.ScheduledAt, &n.TemplateID)
	if err != nil {
		return domain.Notification{}, err
	}
	n.Channel = domain.Channel(channel)
	n.Priority = domain.Priority(priority)
	n.Status = domain.Status(status)
	return n, nil
}
