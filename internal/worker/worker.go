// Package worker claims queued notifications under a lease and delivers them
// to the provider (K2, K3, K6, K21). It defines the store it needs;
// internal/postgres implements it without importing this package (K23).
package worker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
	ClaimNext(ctx context.Context, lease time.Duration) (domain.Claim, bool, error)
	MarkSent(ctx context.Context, id, leaseToken string, acc domain.Acceptance) (bool, error)
	MarkFailed(ctx context.Context, id, leaseToken, lastError string) (bool, error)
}

// Config holds the worker's timing settings (K32).
type Config struct {
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
}

// claimTimeout bounds one claim statement.
const claimTimeout = 5 * time.Second

// ErrShutdownBudget means calls were still in flight when the shutdown budget
// ended. Their leases expire and recovery takes the notifications back.
var ErrShutdownBudget = errors.New("calls still in flight when the shutdown budget ended")

// Worker runs the claim loop.
type Worker struct {
	store    Store
	provider *provider.Client
	logger   *slog.Logger
	cfg      Config
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
// when the queue is empty. When ctx ends it stops claiming at once, lets the
// calls in flight finish and write their results, and returns when they are
// done or ShutdownBudget has passed.
func (w *Worker) Run(ctx context.Context) error {
	slots := make(chan struct{}, w.cfg.InFlight)
	var calls sync.WaitGroup
	w.logger.Info("worker started", slog.Int("in_flight", w.cfg.InFlight),
		slog.String("lease", w.cfg.LeaseDuration.String()))

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
// leave a lease nobody holds.
func (w *Worker) claim(ctx context.Context) (claimed, bool, error) {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), claimTimeout)
	defer cancel()

	t0 := time.Now()
	c, ok, err := w.store.ClaimNext(qctx, w.cfg.LeaseDuration)
	if err != nil || !ok {
		return claimed{}, false, err
	}
	return claimed{Claim: c, deadline: t0.Add(c.LeaseRemaining)}, true, nil
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

	// The call ends ResultBudget before the lease, so no POST runs on an
	// expired lease and the result can still be written (K9). The provider
	// client also applies its HTTP timeout.
	callCtx, cancelCall := context.WithDeadline(context.Background(), c.deadline.Add(-w.cfg.ResultBudget))
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

	// A new context for the write: the call's may have ended. It is bounded
	// by the lease, after which the write could not succeed anyway (K15).
	dbCtx, cancelDB := context.WithDeadline(context.Background(), c.deadline)
	defer cancelDB()

	var (
		outcome   domain.Status
		lastError string
		written   bool
		writeErr  error
	)
	switch {
	case callErr != nil:
		// Interim until slice 4 classifies errors (K7): every failure is final.
		outcome, lastError = domain.StatusFailed, callErr.Error()
		written, writeErr = w.store.MarkFailed(dbCtx, c.ID, c.LeaseToken, lastError)
	case resp.StatusCode == http.StatusAccepted:
		if resp.BodyProblem != "" {
			log.Warn("provider accepted the notification but its response body is not usable; provider fields left empty",
				slog.String("problem", resp.BodyProblem))
		}
		outcome = domain.StatusSent
		written, writeErr = w.store.MarkSent(dbCtx, c.ID, c.LeaseToken, resp.Acceptance)
	default:
		// Interim until slice 4 (K41): every other status, a 3xx included
		// (redirects are not followed), is final.
		outcome, lastError = domain.StatusFailed, fmt.Sprintf("unexpected status %d", resp.StatusCode)
		written, writeErr = w.store.MarkFailed(dbCtx, c.ID, c.LeaseToken, lastError)
	}

	attrs := []slog.Attr{
		slog.Int("status_code", resp.StatusCode),
		slog.Float64("duration_ms", float64(duration.Microseconds())/1000),
		slog.String("outcome", string(outcome)),
	}
	if lastError != "" {
		attrs = append(attrs, slog.String("error", lastError))
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
