package service

import (
	"context"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// StatusStore is what a real-time subscription reads for its snapshot (K24).
type StatusStore interface {
	NotificationStatus(ctx context.Context, id string) (domain.StatusEvent, error)
	BatchStatuses(ctx context.Context, id string) ([]domain.StatusEvent, error)
}

// Statuses reads the snapshots of real-time subscriptions.
type Statuses struct {
	store StatusStore
}

// NewStatuses returns the service.
func NewStatuses(store StatusStore) *Statuses { return &Statuses{store: store} }

// Notification reads one notification's status; domain.ErrNotFound when it
// does not exist.
func (s *Statuses) Notification(ctx context.Context, id string) (domain.StatusEvent, error) {
	return s.store.NotificationStatus(ctx, id)
}

// Batch reads the status of every notification of a batch;
// domain.ErrBatchNotFound when the batch does not exist.
func (s *Statuses) Batch(ctx context.Context, id string) ([]domain.StatusEvent, error) {
	return s.store.BatchStatuses(ctx, id)
}
