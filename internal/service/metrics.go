package service

import (
	"context"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// MetricsStore is what the metrics need from the database.
type MetricsStore interface {
	Metrics(ctx context.Context) (domain.Metrics, error)
}

// Metrics reads the queue and completion snapshot (K20).
type Metrics struct {
	store MetricsStore
}

// NewMetrics returns the metrics service.
func NewMetrics(store MetricsStore) *Metrics {
	return &Metrics{store: store}
}

// Snapshot returns every channel's metrics as of one database snapshot.
func (m *Metrics) Snapshot(ctx context.Context) (domain.Metrics, error) {
	return m.store.Metrics(ctx)
}
