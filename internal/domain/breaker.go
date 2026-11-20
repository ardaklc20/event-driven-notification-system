package domain

import "time"

// BreakerPolicy holds the circuit breaker's settings (K32, K34).
type BreakerPolicy struct {
	// Threshold is the number of outage failures in a row that opens the
	// breaker.
	Threshold int
	// Cooldown is the pause after the breaker opens from closed; each
	// reopening doubles it, up to MaxCooldown.
	Cooldown    time.Duration
	MaxCooldown time.Duration
}

// Attempt names the claim a result write belongs to (K3, K34).
type Attempt struct {
	ID         string
	LeaseToken string
	// Generation is the breaker's generation the claim read.
	Generation int64
	// Probe is true for the one claim taken after the cooldown.
	Probe bool
}

// AttemptOf returns the claim's attempt.
func AttemptOf(c Claim) Attempt {
	return Attempt{ID: c.ID, LeaseToken: c.LeaseToken, Generation: c.Generation, Probe: c.Probe}
}

// ResultKind is what a result write does to the notification (K7).
type ResultKind string

// The three result writes.
const (
	// ResultSent: the provider accepted the notification.
	ResultSent ResultKind = "sent"
	// ResultRetry: back to queued, due Wait after the database clock.
	ResultRetry ResultKind = "retry"
	// ResultFailed: failed on this attempt.
	ResultFailed ResultKind = "failed"
)

// Result is one delivery's result.
type Result struct {
	Kind ResultKind
	// Acceptance is the provider's 202 body, for ResultSent.
	Acceptance Acceptance
	// LastError is the last_error text, for ResultRetry and ResultFailed.
	LastError string
	// Wait is the retry's wait, for ResultRetry.
	Wait time.Duration
	// Outage marks an outage failure (K34): a timeout, a network error, 408
	// or a 5xx that K7 retries.
	Outage bool
	// QuotaPause, when above zero, is the valid Retry-After of a 429 under a
	// profile that states a shared quota: the breaker opens for that long.
	QuotaPause time.Duration
}

// Finish is one result write with the breaker's settings.
type Finish struct {
	Attempt Attempt
	Result  Result
	Policy  BreakerPolicy
}

// BreakerChange is how a result write moved the breaker.
type BreakerChange string

// The breaker changes a result write can make.
const (
	BreakerUnchanged BreakerChange = ""
	BreakerOpened    BreakerChange = "opened"
	BreakerReopened  BreakerChange = "reopened"
	BreakerClosed    BreakerChange = "closed"
)

// Finished is what a result write did.
type Finished struct {
	// Written is false when the lease was no longer valid; then the
	// notification did not change and neither did the breaker.
	Written bool
	Breaker BreakerChange
	// Generation and Cooldown are the breaker's after the change; set only
	// when Breaker is not BreakerUnchanged.
	Generation int64
	Cooldown   time.Duration
	// OpenFor is how long the breaker stays open from the change, when it
	// opened or reopened.
	OpenFor time.Duration
}
