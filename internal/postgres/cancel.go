package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

const (
	// lockForCancelSQL reads the notification under its row lock, so the
	// status cannot change between the decision and the answer (K9).
	lockForCancelSQL = `
		SELECT ` + notificationColumns + `
		FROM notifications
		WHERE id = $1::text::uuid
		FOR UPDATE`

	cancelSQL = `
		UPDATE notifications
		SET status = 'cancelled', updated_at = clock_timestamp()
		WHERE id = $1::text::uuid AND status = 'queued'
		RETURNING ` + notificationColumns
)

// CancelNotification runs the K9 cancel transaction: lock the row, decide
// from the status read under the lock, and cancel only a queued
// notification. It returns the notification as the decision left it. An
// already cancelled one is returned unchanged; a processing, sent or failed
// one comes back with a *domain.CancelConflictError; an unknown ID gives
// domain.ErrNotFound.
func (s *Store) CancelNotification(ctx context.Context, id string) (domain.Notification, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.Notification{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	n, err := cancelIn(ctx, tx, id)
	var conflict *domain.CancelConflictError
	if err != nil && !errors.As(err, &conflict) {
		return domain.Notification{}, err
	}
	if cerr := tx.Commit(ctx); cerr != nil {
		return domain.Notification{}, fmt.Errorf("commit: %w", cerr)
	}
	return n, err
}

// cancelIn is the cancel's decision on tx, which the caller commits. Tests
// run it on transactions they hold to set up each race order.
func cancelIn(ctx context.Context, tx pgx.Tx, id string) (domain.Notification, error) {
	n, err := scanNotification(tx.QueryRow(ctx, lockForCancelSQL, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Notification{}, domain.ErrNotFound
	}
	if err != nil {
		return domain.Notification{}, fmt.Errorf("lock for cancel: %w", err)
	}
	switch n.Status {
	case domain.StatusQueued:
		n, err = scanNotification(tx.QueryRow(ctx, cancelSQL, id))
		if err != nil {
			return domain.Notification{}, fmt.Errorf("cancel: %w", err)
		}
		return n, nil
	case domain.StatusCancelled:
		return n, nil
	default:
		return n, &domain.CancelConflictError{Status: n.Status}
	}
}
