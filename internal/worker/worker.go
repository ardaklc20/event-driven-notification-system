// Package worker claims queued notifications under a lease, delivers them to
// the provider, retries temporary failures and takes back expired leases (K2,
// K3, K6, K7, K21, K40). It defines the store it needs; internal/postgres
// implements it without importing this package (K23).
package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
)

// Store is what the worker needs from the database. Every result write is
// conditioned on the claim's lease and reports false when the lease was no
// longer valid (K3).
type Store interface {
	ClaimNext(ctx context.Context, lease time.Duration, maxAttempts int) (domain.Claim, bool, error)
	MarkSent(ctx context.Context, id, leaseToken string, acc domain.Acceptance) (bool, error)
	MarkRetry(ctx context.Context, id, leaseToken, lastError string, wait time.Duration) (bool, error)
	MarkFailed(ctx context.Context, id, leaseToken, lastError string) (bool, error)
	RecoverExpired(ctx context.Context, maxAttempts, batch int) ([]domain.Recovered, error)
}

// Config holds the worker's settings (K32).
type Config struct {
	// Profile decides how a 429 is classified (K7, K14).
	Profile provider.Profile
	// LeaseDuration is how long a claim holds its notification.
	LeaseDuration time.Duration
	// ResultBudget is the part of the lease kept for writing the result: the
	// provider call ends at least this long before the lease does.
	ResultBudget time.Duration
	// InFlight is the number of provider calls this worker runs at once.
	InFlight int
	// IdleWait is the pause after a claim that found nothing.
	IdleWait time.Duration
	// ShutdownBudget bounds the wait for calls in flight after a stop (K6).
	ShutdownBudget time.Duration
	// MaxAttempts counts the first attempt too (K7).
	MaxAttempts int
	// BackoffBase and BackoffCap shape the full-jitter backoff (K7).
	BackoffBase time.Duration
	BackoffCap  time.Duration
	// RecoveryInterval and RecoveryBatch bound the recovery step (K40).
	RecoveryInterval time.Duration
	RecoveryBatch    int
}

// queryTimeout bounds one claim or recovery statement.
const queryTimeout = 5 * time.Second

// ErrShutdownBudget means calls were still in flight when the shutdown budget
// ended. Their leases expire and recovery takes the notifications back.
var ErrShutdownBudget = errors.New("calls still in flight when the shutdown budget ended")

// Worker runs the claim loop and the recovery step.
type Worker struct {
	store    Store
	provider *provider.Client
	logger   *slog.Logger
	cfg      Config
	hooks    hooks
}

// hooks let tests stop a delivery at a chosen point to simulate a crash. They
// are nil in production; export_test.go sets them.
type hooks struct {
	afterClaim  func(domain.Claim)
	beforeWrite func(domain.Claim)
}

// New returns a worker. logger should already carry the worker_id.
func New(store Store, client *provider.Client, logger *slog.Logger, cfg Config) *Worker {
	return &Worker{store: store, provider: client, logger: logger, cfg: cfg}
}

// NewID returns a worker ID: the host name and a random suffix.
func NewID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "worker"
	}
	return host + "-" + strings.ToLower(rand.Text()[:8])
}

// claimed is a claim with its local lease deadline.
type claimed struct {
	domain.Claim
	// deadline is t0 + LeaseRemaining, where t0 was read just before the claim
	// was sent. t0 comes before the moment the database computed the remaining
	// time, so this deadline falls at or before the database's lease end.
	deadline time.Time
}

// Run claims while it finds work and a call slot is free, and waits IdleWait
// when the queue is empty. Beside the claim loop it runs the recovery step
// every RecoveryInterval (K40). When ctx ends it stops claiming and
// recovering at once, lets the calls in flight finish and write their results,
// and returns when they are done or ShutdownBudget has passed.
func (w *Worker) Run(ctx context.Context) error {
	slots := make(chan struct{}, w.cfg.InFlight)
	var calls, background sync.WaitGroup
	w.logger.Info("worker started", slog.Int("in_flight", w.cfg.InFlight),
		slog.String("lease", w.cfg.LeaseDuration.String()), slog.Int("max_attempts", w.cfg.MaxAttempts),
		slog.String("profile", string(w.cfg.Profile)))
	background.Go(func() { w.recoverLoop(ctx) })

loop:
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		// select picks at random when a slot and the stop are both ready, so
		// the stop is checked again: no claim after a stop.
		if ctx.Err() != nil {
			<-slots
			break loop
		}

		c, ok, err := w.claim(ctx)
		if err != nil || !ok {
			<-slots
			if err != nil {
				w.logger.Error("claim failed", slog.String("error", err.Error()))
			}
			select {
			case <-time.After(w.cfg.IdleWait):
			case <-ctx.Done():
				break loop
			}
			continue
		}
		calls.Go(func() {
			defer func() { <-slots }()
			w.deliver(c)
		})
	}

	w.logger.Info("stop requested, no new claims", slog.Int("calls_in_flight", len(slots)),
		slog.String("budget", w.cfg.ShutdownBudget.String()))
	done := make(chan struct{})
	go func() {
		calls.Wait()
		background.Wait()
		close(done)
	}()
	select {
	case <-done:
		w.logger.Info("worker stopped")
		return nil
	case <-time.After(w.cfg.ShutdownBudget):
		w.logger.Error("shutdown budget ended with calls in flight", slog.Int("calls_in_flight", len(slots)))
		return ErrShutdownBudget
	}
}

// claim reads the monotonic clock just before it sends the claim and sets the
// lease deadline from that reading (K3). The claim statement is not cancelled
// by a stop: cancelling a statement that may already have committed would
// leave a lease nobody holds until recovery.
func (w *Worker) claim(ctx context.Context) (claimed, bool, error) {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()

	t0 := time.Now()
	c, ok, err := w.store.ClaimNext(qctx, w.cfg.LeaseDuration, w.cfg.MaxAttempts)
	if err != nil || !ok {
		return claimed{}, false, err
	}
	return claimed{Claim: c, deadline: t0.Add(c.LeaseRemaining)}, true, nil
}

// recoverLoop runs the recovery step at once and then every RecoveryInterval
// until ctx ends (K40).
func (w *Worker) recoverLoop(ctx context.Context) {
	ticker := time.NewTicker(w.cfg.RecoveryInterval)
	defer ticker.Stop()
	for {
		w.recoverOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// recoverOnce takes back at most RecoveryBatch expired leases. A stop may
// cancel it: recovery only changes statuses, and the next round or another
// worker repeats it.
func (w *Worker) recoverOnce(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	recovered, err := w.store.RecoverExpired(qctx, w.cfg.MaxAttempts, w.cfg.RecoveryBatch)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Error("lease recovery failed", slog.String("error", err.Error()))
		}
		return
	}
	for _, r := range recovered {
		w.logger.Warn("lease expired, notification taken back",
			slog.String("correlation_id", r.CorrelationID),
			slog.String("notification_id", r.ID),
			slog.Int("attempts", r.Attempts),
			slog.String("outcome", string(r.Status)))
	}
}

// deliver sends one claimed notification and writes its result. Its contexts
// do not come from Run's: a stop lets the call finish.
func (w *Worker) deliver(c claimed) {
	log := w.logger.With(
		slog.String("correlation_id", c.CorrelationID),
		slog.String("notification_id", c.ID),
		slog.String("channel", string(c.Channel)),
		slog.Int("attempt", c.Attempt),
	)
	if w.hooks.afterClaim != nil {
		w.hooks.afterClaim(c.Claim)
	}

	// The call ends ResultBudget before the lease, so no POST runs on an
	// expired lease and the result can still be written (K9). The provider
	// client also applies its HTTP timeout.
	callDeadline := c.deadline.Add(-w.cfg.ResultBudget)
	if !time.Now().Before(callDeadline) {
		// Too little lease is left to send and write a result. The attempt
		// stays counted; recovery takes the notification back (K9).
		log.Warn("lease too short to send; no provider call, left for recovery",
			slog.String("lease_left", time.Until(c.deadline).Round(time.Millisecond).String()))
		return
	}
	callCtx, cancelCall := context.WithDeadline(context.Background(), callDeadline)
	defer cancelCall()

	log.Info("provider call started")
	start := time.Now()
	resp, callErr := w.provider.Send(callCtx, provider.Request{
		To:            c.Recipient,
		Channel:       string(c.Channel),
		Content:       c.Content,
		CorrelationID: c.CorrelationID,
	})
	duration := time.Since(start)
	outcome := provider.Classify(resp, callErr, w.cfg.Profile)

	if w.hooks.beforeWrite != nil {
		w.hooks.beforeWrite(c.Claim)
	}

	// A new context for the write: the call's may have ended. It is bounded
	// by the lease, after which the write could not succeed anyway (K15).
	dbCtx, cancelDB := context.WithDeadline(context.Background(), c.deadline)
	defer cancelDB()

	attrs := []slog.Attr{
		slog.Int("status_code", resp.StatusCode),
		slog.Float64("duration_ms", float64(duration.Microseconds())/1000),
		slog.String("class", string(outcome.Class)),
	}
	var (
		status   domain.Status
		written  bool
		writeErr error
	)
	switch {
	case outcome.Class == provider.ClassAccepted:
		if resp.BodyProblem != "" {
			log.Warn("provider accepted the notification but its response body is not usable; provider fields left empty",
				slog.String("problem", resp.BodyProblem))
		}
		status = domain.StatusSent
		written, writeErr = w.store.MarkSent(dbCtx, c.ID, c.LeaseToken, resp.Acceptance)
	case outcome.Class == provider.ClassTemporary && c.Attempt < w.cfg.MaxAttempts:
		retryAfter, hasRetryAfter := provider.ParseRetryAfter(resp.RetryAfter, time.Now())
		wait := NextWait(c.Attempt, w.cfg.BackoffBase, w.cfg.BackoffCap, retryAfter, hasRetryAfter)
		status = domain.StatusQueued
		attrs = append(attrs, slog.Float64("retry_in_ms", float64(wait.Microseconds())/1000))
		written, writeErr = w.store.MarkRetry(dbCtx, c.ID, c.LeaseToken, outcome.Error, wait)
	default:
		// Permanent, terminal (K41), or temporary on the last attempt.
		status = domain.StatusFailed
		written, writeErr = w.store.MarkFailed(dbCtx, c.ID, c.LeaseToken, outcome.Error)
	}

	attrs = append(attrs, slog.String("outcome", string(status)))
	if outcome.Error != "" {
		attrs = append(attrs, slog.String("error", outcome.Error))
	}
	switch {
	case writeErr != nil:
		log.LogAttrs(dbCtx, slog.LevelError, "provider call finished, result not written",
			append(attrs, slog.String("write_error", writeErr.Error()))...)
	case !written:
		log.LogAttrs(dbCtx, slog.LevelWarn, "provider call finished, lease lost, result not written", attrs...)
	default:
		log.LogAttrs(dbCtx, slog.LevelInfo, "provider call finished", attrs...)
	}
}
