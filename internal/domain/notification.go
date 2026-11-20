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

// Channels lists the three channels; each has its own rate limit (K5).
var Channels = []Channel{ChannelSMS, ChannelEmail, ChannelPush}

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
	// PermitRemaining is the time left until the permit's start deadline,
	// computed by the database in the same way (K42). The send must start
	// before that deadline.
	PermitRemaining time.Duration
}

// ClaimRequest is one claim attempt on one channel (K5, K42).
type ClaimRequest struct {
	Channel     Channel
	Lease       time.Duration
	MaxAttempts int
	// PermitInterval is the least time between two permits of the channel.
	PermitInterval time.Duration
	// PermitGrace is how long after its time a permit may start a send.
	PermitGrace time.Duration
}

// ClaimOutcome is what a claim attempt found (K5).
type ClaimOutcome string

// The four claim outcomes.
const (
	// ClaimClaimed: a notification and a permit.
	ClaimClaimed ClaimOutcome = "claimed"
	// ClaimThrottled: the channel's next permit is not due yet.
	ClaimThrottled ClaimOutcome = "throttled"
	// ClaimBusy: another worker holds the channel's limiter row.
	ClaimBusy ClaimOutcome = "busy"
	// ClaimEmpty: the permit is due, but no notification is; the permit was
	// not used.
	ClaimEmpty ClaimOutcome = "empty"
)

// ClaimResult is the answer to one claim attempt.
type ClaimResult struct {
	Outcome ClaimOutcome
	// Claim is set when Outcome is ClaimClaimed.
	Claim Claim
	// Wait is the time until the next permit is due, computed by the
	// database, when Outcome is ClaimThrottled.
	Wait time.Duration
}

// Recovered is a notification whose expired lease recovery took back (K3, K9,
// K40): queued again while attempts are left, otherwise failed.
type Recovered struct {
	ID            string
	CorrelationID string
	Status        Status
	Attempts      int
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

// ListFilter is one listing query (K13). Empty fields do not filter.
type ListFilter struct {
	Status  Status
	Channel Channel
	// From is included and To excluded: the range is half-open.
	From, To *time.Time
	// After, when set, returns only rows below it in the listing order.
	After *Cursor
	// Limit is the most rows returned.
	Limit int
}

// Cursor is a position in the listing order: created_at DESC, id DESC.
type Cursor struct {
	CreatedAt time.Time
	ID        string
}

// ErrInvalidCursor means a cursor was not produced by a previous page.
var ErrInvalidCursor = errors.New("cursor is not valid")

// CancelConflictError means the notification can no longer be cancelled: a
// worker holds it (processing) or it has ended (sent, failed). Status is the
// status read under the row lock (K9).
type CancelConflictError struct {
	Status Status
}

func (e *CancelConflictError) Error() string {
	return "notification is " + string(e.Status) + "; only a queued notification can be cancelled"
}

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
