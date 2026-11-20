package domain

import "time"

// StatusEvent is one notification's status as a real-time subscriber sees it
// (K24): the trigger's payload, and each item of a snapshot. Version grows by
// one on every status change of that notification; versions of different
// notifications are never compared.
type StatusEvent struct {
	NotificationID string
	// BatchID is nil for a single create.
	BatchID   *string
	Status    Status
	Attempts  int
	Version   int64
	UpdatedAt time.Time
}
