package postgres

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

const (
	insertBatchSQL = `
		INSERT INTO batches (item_count, correlation_id)
		VALUES ($1, $2)
		RETURNING id::text`

	getBatchSQL = `
		SELECT id::text, item_count, correlation_id, created_at
		FROM batches
		WHERE id = $1::text::uuid`

	countBatchSQL = `
		SELECT status::text, count(*)
		FROM notifications
		WHERE batch_id = $1::text::uuid
		GROUP BY status`
)

// insertItemsSQL returns one multi-row INSERT for n items. $1 is the
// correlation ID and $2 the batch ID, shared by every row; each item adds five
// parameters, so 1,000 items use 5,002, below the protocol's 65,535 (K17).
func insertItemsSQL(n int) string {
	var b strings.Builder
	b.WriteString(`INSERT INTO notifications
		(recipient, channel, content, priority, batch_position, correlation_id, batch_id) VALUES `)
	for i := range n {
		if i > 0 {
			b.WriteString(", ")
		}
		p := 3 + i*5
		fmt.Fprintf(&b, "($%d, $%d, $%d, $%d::text::notification_priority, $%d, $1, $2::text::uuid)",
			p, p+1, p+2, p+3, p+4)
	}
	b.WriteString(" RETURNING id::text, batch_position")
	return b.String()
}

// CreateBatch saves a batch under the K10 and K12 rules, in one transaction:
// reserve the key, check the content limits, insert the batch row, insert
// every item with one statement, store the 202 bytes with the key, commit.
// Any failure rolls everything back: no batch, no notification, no key.
func (s *Store) CreateBatch(ctx context.Context, cmd domain.BatchCommand) (domain.CreateResult, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	if cmd.Key != "" {
		reserved, err := reserveKey(ctx, tx, cmd.Key, domain.OperationBatch, cmd.RequestHash)
		if err != nil {
			return domain.CreateResult{}, err
		}
		if !reserved {
			if err := tx.Rollback(ctx); err != nil {
				return domain.CreateResult{}, fmt.Errorf("rollback: %w", err)
			}
			return s.replay(ctx, cmd.Key, domain.OperationBatch, cmd.RequestHash)
		}
	}

	if cmd.CheckLimits != nil {
		if err := cmd.CheckLimits(); err != nil {
			return domain.CreateResult{}, err
		}
	}

	var batchID string
	if err := tx.QueryRow(ctx, insertBatchSQL, len(cmd.Items), cmd.CorrelationID).Scan(&batchID); err != nil {
		return domain.CreateResult{}, fmt.Errorf("insert batch: %w", err)
	}

	args := make([]any, 0, 2+5*len(cmd.Items))
	args = append(args, cmd.CorrelationID, batchID)
	for i, n := range cmd.Items {
		args = append(args, n.Recipient, string(n.Channel), n.Content, string(n.Priority), i)
	}
	rows, err := tx.Query(ctx, insertItemsSQL(len(cmd.Items)), args...)
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("insert items: %w", err)
	}
	// RETURNING has no documented order, so each ID goes to its position.
	ids := make([]string, len(cmd.Items))
	var (
		id       string
		position int
	)
	if _, err := pgx.ForEachRow(rows, []any{&id, &position}, func() error {
		if position < 0 || position >= len(ids) || ids[position] != "" {
			return fmt.Errorf("unexpected batch_position %d", position)
		}
		ids[position] = id
		return nil
	}); err != nil {
		return domain.CreateResult{}, fmt.Errorf("insert items: %w", err)
	}
	for i, id := range ids {
		if id == "" {
			return domain.CreateResult{}, fmt.Errorf("insert items: no row returned for position %d", i)
		}
	}

	body, err := cmd.Respond(batchID, ids)
	if err != nil {
		return domain.CreateResult{}, fmt.Errorf("build response: %w", err)
	}
	if cmd.Key != "" {
		if _, err := tx.Exec(ctx, completeKeySQL, cmd.Key, http.StatusAccepted, body, nil); err != nil {
			return domain.CreateResult{}, fmt.Errorf("store response: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.CreateResult{}, fmt.Errorf("commit: %w", err)
	}
	return domain.CreateResult{Status: http.StatusAccepted, Body: body}, nil
}

// GetBatch returns the batch with its counts per status and one page of its
// notifications (f.BatchID is set from id). The counts and the page are read
// in one REPEATABLE READ transaction, so both come from the same snapshot:
// a worker that changes a status meanwhile changes neither (K12, K13).
func (s *Store) GetBatch(ctx context.Context, id string, f domain.ListFilter) (domain.Batch, []domain.Notification, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return domain.Batch{}, nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var b domain.Batch
	err = tx.QueryRow(ctx, getBatchSQL, id).Scan(&b.ID, &b.ItemCount, &b.CorrelationID, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Batch{}, nil, domain.ErrBatchNotFound
	}
	if err != nil {
		return domain.Batch{}, nil, fmt.Errorf("get batch: %w", err)
	}

	b.Counts = make(map[domain.Status]int, len(domain.Statuses))
	for _, st := range domain.Statuses {
		b.Counts[st] = 0
	}
	rows, err := tx.Query(ctx, countBatchSQL, id)
	if err != nil {
		return domain.Batch{}, nil, fmt.Errorf("count batch: %w", err)
	}
	var (
		status string
		count  int
	)
	if _, err := pgx.ForEachRow(rows, []any{&status, &count}, func() error {
		b.Counts[domain.Status(status)] = count
		return nil
	}); err != nil {
		return domain.Batch{}, nil, fmt.Errorf("count batch: %w", err)
	}

	f.BatchID = b.ID
	items, err := listOn(ctx, tx, f)
	if err != nil {
		return domain.Batch{}, nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Batch{}, nil, fmt.Errorf("commit: %w", err)
	}
	return b, items, nil
}
