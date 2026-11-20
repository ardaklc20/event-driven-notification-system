// Package service holds the create, read and (later) cancel, list and metrics
// rules. It defines the store interface it needs; internal/postgres implements
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
}

// Create follows the K10 order: structure rules and defaults, the request hash,
// then the store's transaction, which checks the content limits only when it
// is about to save a new notification. respond builds the 202 body; the store
// keeps its bytes with the key.
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
		RequestHash:   domain.RequestHash(domain.OperationCreate, []domain.NewNotification{n}),
		CheckLimits: func() error {
			if errs := s.limits.Check(n); len(errs) > 0 {
				return &domain.ValidationError{Errors: errs}
			}
			return nil
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
	f := domain.ListFilter{Status: in.Status, Channel: in.Channel, From: in.From, To: in.To, Limit: in.Limit + 1}
	if in.Cursor != "" {
		c, err := DecodeCursor(in.Cursor)
		if err != nil {
			return ListResult{}, err
		}
		f.After = &c
	}
	items, err := s.store.ListNotifications(ctx, f)
	if err != nil {
		return ListResult{}, err
	}
	if len(items) <= in.Limit {
		return ListResult{Items: items}, nil
	}
	items = items[:in.Limit]
	last := items[len(items)-1]
	return ListResult{Items: items, NextCursor: EncodeCursor(domain.Cursor{CreatedAt: last.CreatedAt, ID: last.ID})}, nil
}
