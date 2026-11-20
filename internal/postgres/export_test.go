package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// ClaimSQL is the claim statement, for tests that inspect its plan or run it
// on a transaction they hold.
const ClaimSQL = claimSQL

// MarkRetrySQL is the processing → queued result write, for race tests that
// run it on a transaction they hold. Its arguments: id, lease token, last
// error, wait in microseconds.
const MarkRetrySQL = markRetrySQL

// CancelIn runs the cancel's decision on tx without committing it.
func CancelIn(ctx context.Context, tx pgx.Tx, id string) (domain.Notification, error) {
	return cancelIn(ctx, tx, id)
}

// ListQuery is the listing query for f, for the test that inspects its plan.
func ListQuery(f domain.ListFilter) (string, []any) {
	return listQuery(f)
}
