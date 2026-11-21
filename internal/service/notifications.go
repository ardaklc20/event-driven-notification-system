// Package service holds the create, read, cancel, list, batch and (later)
// metrics rules. It defines the store interface it needs; internal/postgres implements
// it without importing this package (K23).
package service

import (
	"context"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// Store is what the notification rules need from the database.
type Store interface {
	CreateNotification(ctx context.Context, cmd domain.CreateCommand) (domain.CreateResult, error)
	GetNotification(ctx context.Context, id string) (domain.Notification, error)
	CancelNotification(ctx context.Context, id string) (domain.Notification, error)
	ListNotifications(ctx context.Context, f domain.ListFilter) ([]domain.Notification, error)
	CreateBatch(ctx context.Context, cmd domain.BatchCommand) (domain.CreateResult, error)
	GetBatch(ctx context.Context, id string, f domain.ListFilter) (domain.Batch, []domain.Notification, error)
}

// Notifications creates, reads, cancels and lists notifications.
type Notifications struct {
	store  Store
	limits domain.Limits
}

// NewNotifications returns the service with the configured content limits.
func NewNotifications(store Store, limits domain.Limits) *Notifications {
	return &Notifications{store: store, limits: limits}
}

// CreateInput is one create request.
type CreateInput struct {
	Notification  domain.NewNotification
	Key           string // Idempotency-Key; empty when the request has none
	CorrelationID string
	// Traceparent is the API request's server span context, or "" (K27).
	Traceparent string
}

// Create follows the K10 order: structure rules and defaults, the request hash,
// then the store's transaction, which renders a template request and checks
// the content limits only when it is about to save a new notification (K33).
// respond builds the 202 body; the store keeps its bytes with the key.
func (s *Notifications) Create(ctx context.Context, in CreateInput, respond func(domain.Notification) ([]byte, error)) (domain.CreateResult, error) {
	n, errs := domain.Normalize(in.Notification)
	if len(errs) > 0 {
		return domain.CreateResult{}, &domain.ValidationError{Errors: errs}
	}
	return s.store.CreateNotification(ctx, domain.CreateCommand{
		Operation:     domain.OperationCreate,
		Notification:  n,
		CorrelationID: in.CorrelationID,
		Key:           in.Key,
		Traceparent:   in.Traceparent,
		RequestHash:   domain.RequestHash(domain.OperationCreate, []domain.NewNotification{n}),
		Prepare: func(templates domain.Templates) (domain.NewNotification, error) {
			out, errs := s.limits.Prepare(n, templates)
			if len(errs) > 0 {
				return domain.NewNotification{}, &domain.ValidationError{Errors: errs}
			}
			return out, nil
		},
		Respond: respond,
	})
}

// Get reads one notification; domain.ErrNotFound when it does not exist.
func (s *Notifications) Get(ctx context.Context, id string) (domain.Notification, error) {
	return s.store.GetNotification(ctx, id)
}

// Cancel cancels a queued notification (K9). It returns the notification as
// the decision left it: cancelled now, or already cancelled. A notification a
// worker holds or that has ended gives a *domain.CancelConflictError with its
// status; an unknown ID gives domain.ErrNotFound.
func (s *Notifications) Cancel(ctx context.Context, id string) (domain.Notification, error) {
	return s.store.CancelNotification(ctx, id)
}

// ListInput is one listing request (K13). Cursor is the opaque next_cursor of
// the previous page, or "".
type ListInput struct {
	Status   domain.Status
	Channel  domain.Channel
	From, To *time.Time
	Cursor   string
	Limit    int
}

// ListResult is one page.
type ListResult struct {
	Items []domain.Notification
	// NextCursor reads the next page; "" on the last page.
	NextCursor string
}

// List returns one page, newest first. It reads one row more than the page
// holds to learn whether another page exists; the next cursor is built from
// the last returned row. A cursor that does not decode gives
// domain.ErrInvalidCursor.
func (s *Notifications) List(ctx context.Context, in ListInput) (ListResult, error) {
	f, err := listFilter(in.Cursor, in.Limit)
	if err != nil {
		return ListResult{}, err
	}
	f.Status, f.Channel, f.From, f.To = in.Status, in.Channel, in.From, in.To
	items, err := s.store.ListNotifications(ctx, f)
	if err != nil {
		return ListResult{}, err
	}
	return page(items, in.Limit), nil
}

// listFilter returns the filter that reads one row more than limit, after
// the decoded cursor.
func listFilter(cursor string, limit int) (domain.ListFilter, error) {
	f := domain.ListFilter{Limit: limit + 1}
	if cursor != "" {
		c, err := DecodeCursor(cursor)
		if err != nil {
			return domain.ListFilter{}, err
		}
		f.After = &c
	}
	return f, nil
}

// page cuts the extra row read to learn whether another page exists and
// builds the next cursor from the last returned row.
func page(items []domain.Notification, limit int) ListResult {
	if len(items) <= limit {
		return ListResult{Items: items}
	}
	items = items[:limit]
	last := items[len(items)-1]
	return ListResult{Items: items, NextCursor: EncodeCursor(domain.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})}
}

// BatchInput is one batch create request (K12).
type BatchInput struct {
	Items         []domain.NewNotification
	Key           string
	CorrelationID string
	// Traceparent is the batch request's server span context, or "" (K27).
	Traceparent string
}

// CreateBatch follows the order of Create for every item: structure rules and
// defaults for all items with every error collected, the request hash over
// all normalized items, then the store's transaction, which renders and
// checks every item only when it is about to save them. respond
// builds the 202 body from the batch ID and the IDs in request order.
func (s *Notifications) CreateBatch(ctx context.Context, in BatchInput,
	respond func(batchID string, ids []string) ([]byte, error)) (domain.CreateResult, error) {
	if len(in.Items) == 0 || len(in.Items) > domain.MaxBatchItems {
		return domain.CreateResult{}, &domain.ValidationError{Errors: []domain.FieldError{{
			Field: "notifications", Code: domain.CodeUnknownValue, Message: "must hold 1 to 1000 items",
		}}}
	}
	items, errs := domain.NormalizeBatch(in.Items)
	if len(errs) > 0 {
		return domain.CreateResult{}, &domain.ValidationError{Errors: errs}
	}
	return s.store.CreateBatch(ctx, domain.BatchCommand{
		Items:         items,
		CorrelationID: in.CorrelationID,
		Key:           in.Key,
		Traceparent:   in.Traceparent,
		RequestHash:   domain.RequestHash(domain.OperationBatch, items),
		Prepare: func(templates domain.Templates) ([]domain.NewNotification, error) {
			out := make([]domain.NewNotification, len(items))
			var errs []domain.FieldError
			for i, n := range items {
				prepared, itemErrs := s.limits.Prepare(n, templates)
				out[i] = prepared
				errs = append(errs, domain.ItemErrors(i, itemErrs)...)
			}
			if len(errs) > 0 {
				return nil, &domain.ValidationError{Errors: errs}
			}
			return out, nil
		},
		Respond: respond,
	})
}

// BatchResult is a batch with one page of its notifications.
type BatchResult struct {
	Batch domain.Batch
	Page  ListResult
}

// GetBatch returns the batch's counts per status and one page of its
// notifications in the listing order (K12, K13); domain.ErrBatchNotFound for
// an unknown ID, domain.ErrInvalidCursor for a cursor no page produced.
func (s *Notifications) GetBatch(ctx context.Context, id, cursor string, limit int) (BatchResult, error) {
	f, err := listFilter(cursor, limit)
	if err != nil {
		return BatchResult{}, err
	}
	b, items, err := s.store.GetBatch(ctx, id, f)
	if err != nil {
		return BatchResult{}, err
	}
	return BatchResult{Batch: b, Page: page(items, limit)}, nil
}
