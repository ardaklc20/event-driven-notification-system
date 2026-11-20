package postgres

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// listQuery builds the listing query for f (K13). Each filter that is set
// adds one fixed condition with a numbered parameter; values never enter the
// SQL text (K17). Without filters the query is a scan of
// notifications_list_idx in its order.
func listQuery(f domain.ListFilter) (string, []any) {
	var (
		where []string
		args  []any
	)
	param := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.Status != "" {
		where = append(where, "status = "+param(string(f.Status))+"::text::notification_status")
	}
	if f.Channel != "" {
		where = append(where, "channel = "+param(string(f.Channel)))
	}
	if f.BatchID != "" {
		where = append(where, "notifications.batch_id = "+param(f.BatchID)+"::text::uuid")
	}
	if f.From != nil {
		where = append(where, "created_at >= "+param(*f.From))
	}
	if f.To != nil {
		where = append(where, "created_at < "+param(*f.To))
	}
	if f.After != nil {
		where = append(where, "(notifications.created_at, notifications.id) < ("+
			param(f.After.CreatedAt)+", "+param(f.After.ID)+"::text::uuid)")
	}
	q := "SELECT " + notificationColumns + " FROM notifications"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// Qualified names: a bare id would sort by the selected id::text column
	// and keep the index from providing the order.
	q += " ORDER BY notifications.created_at DESC, notifications.id DESC LIMIT " + param(f.Limit)
	return q, args
}

// ListNotifications returns at most f.Limit notifications that match f,
// newest first by (created_at, id); with f.After, only those below it.
func (s *Store) ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error) {
	return listOn(ctx, s.pool, f)
}

// querier is a pool or a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func listOn(ctx context.Context, db querier, f domain.ListFilter) ([]domain.Notification, error) {
	q, args := listQuery(f)
	rows, err := db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (domain.Notification, error) {
		return scanNotification(row)
	})
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}
	return out, nil
}
