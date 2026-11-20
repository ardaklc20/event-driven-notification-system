// Package domain holds the notification types, the shared validation (K11)
// and the request hash (K10). It has no database or HTTP code.
package domain

import (
	"errors"
	"time"
)

// Channel is where a notification is delivered.
type Channel string

// The three channels.
const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
)

// Priority orders work within a channel (K8). The database enum declares the
// same values in this order (K37).
type Priority string

// The three priorities, highest first.
const (
	PriorityHigh   Priority = "high"
	PriorityNormal Priority = "normal"
	PriorityLow    Priority = "low"
)

// Status is a notification's place in its life cycle (K9).
type Status string

// The five statuses.
const (
	StatusQueued     Status = "queued"
	StatusProcessing Status = "processing"
	StatusSent       Status = "sent"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

// Operation names a create endpoint. It is part of the request hash, so the
// same body sent to the other endpoint never matches (K10).
type Operation string

// The two create operations.
const (
	OperationCreate Operation = "create"
	OperationBatch  Operation = "batch"
)

// NewNotification is a create request before it is saved.
type NewNotification struct {
	Recipient string
	Channel   Channel
	Content   string
	Priority  Priority
}

// Notification is a saved notification.
type Notification struct {
	ID            string
	Recipient     string
	Channel       Channel
	Content       string
	Priority      Priority
	Status        Status
	Attempts      int
	LastError     *string
	CorrelationID string
	// What the provider answered with its 202, when it was readable (K15).
	ProviderMessageID *string
	ProviderTimestamp *time.Time
	// SentAt is when the acceptance was recorded.
	SentAt    *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Claim is a notification a worker holds under a lease (K3). The worker needs
// only these fields to send it.
type Claim struct {
	ID            string
	Recipient     string
	Channel       Channel
	Content       string
	CorrelationID string
	// Attempt is the attempt this claim started; the claim counted it (K9).
	Attempt int
	// LeaseToken must match for any result write (K3).
	LeaseToken string
	// LeaseRemaining is the lease time left, computed by the database when it
	// returned the claim. The worker adds it to the time it took before sending
	// the claim, never to the time the answer arrived.
	LeaseRemaining time.Duration
}

// Acceptance is the provider's 202 body. A field the provider sent broken is
// left empty: MessageID "" or a zero Timestamp (K15).
type Acceptance struct {
	MessageID string
	Timestamp time.Time
}

// Errors the store and the service return.
var (
	ErrNotFound = errors.New("notification not found")
	// ErrIdempotencyConflict means the key was already used with a different
	// request (K10).
	ErrIdempotencyConflict = errors.New("idempotency key already used with a different request")
)

// CreateCommand is what the store needs to save one notification under the
// K10 rules, in one transaction.
type CreateCommand struct {
	Operation     Operation
	Notification  NewNotification
	CorrelationID string
	// Key is the Idempotency-Key; empty means the request has none.
	Key         string
	RequestHash []byte
	// CheckLimits runs only when this command is about to write a new
	// notification: when it reserved the key, or has no key. An error rolls
	// the transaction back, so the key stays free.
	CheckLimits func() error
	// Respond builds the response body. Its bytes are stored with the key and
	// sent unchanged on every replay.
	Respond func(Notification) ([]byte, error)
}

// CreateResult is the response to send for a create command.
type CreateResult struct {
	NotificationID string
	Status         int
	Body           []byte
	// Replayed is true when the key was already completed by an earlier
	// request and the stored response is returned.
	Replayed bool
}
