// Package service holds the create, read and (later) cancel, list and metrics
// rules. It defines the store interface it needs; internal/postgres implements
// it without importing this package (K23).
package service

import (
	"context"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// Store is what the notification rules need from the database.
type Store interface {
	CreateNotification(ctx context.Context, cmd domain.CreateCommand) (domain.CreateResult, error)
	GetNotification(ctx context.Context, id string) (domain.Notification, error)
}

// Notifications creates and reads notifications.
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
