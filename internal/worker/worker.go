// Package worker claims queued notifications under a lease and a channel
// permit, delivers them to the provider, retries temporary failures and takes
// back expired leases (K2, K3, K5, K6, K7, K8, K21, K40, K42). While the
// provider's circuit breaker is open, it claims nothing but the one probe
// (K34). It defines the store it needs; internal/postgres implements it
// without importing this package (K23).
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
// longer valid (K3). Finish also applies the circuit breaker's rules to the
// result, and GiveBack frees a probe's slot (K34).
type Store interface {
	ClaimNext(ctx context.Context, req domain.ClaimRequest) (domain.ClaimResult, error)
	Finish(ctx context.Context, f domain.Finish) (domain.Finished, error)
	GiveBack(ctx context.Context, a domain.Attempt) (bool, error)
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
	// IdleWait is the pause on a channel after a claim found its permit due
	// but no notification.
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
	// PermitInterval is the least time between two permits of a channel, and
	// PermitGrace how long after its time a permit may start a send (K5, K42).
	PermitInterval time.Duration
	PermitGrace    time.Duration
	// Breaker holds the circuit breaker's settings (K34).
	Breaker domain.BreakerPolicy
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

// hooks let tests stop a delivery at a chosen point to simulate a crash or a
// pause. They are nil in production; export_test.go sets them.
type hooks struct {
	afterClaim  func(domain.Claim)
	beforeGate  func(domain.Claim)
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

// claimed is a claim with its local deadlines.
type claimed struct {
	domain.Claim
	// deadline is t0 + LeaseRemaining, where t0 was read just before the claim
	// was sent. t0 comes before the moment the database computed the remaining
	// time, so this deadline falls at or before the database's lease end.
	deadline time.Time
	// startBy is t0 + PermitRemaining, the permit's start deadline, read the
	// same way (K42).
	startBy time.Time
}

// attempt is the answer to one claim attempt, with the local deadlines when
// it claimed a notification.
type attempt struct {
	domain.ClaimResult
	claimed claimed
	// sent is when the claim was sent (t0).
	sent time.Time
}

// Run claims on each channel when its next try has come and a call slot is
// free. Each claim attempt sets that channel's next try from its outcome: at
// once after a claim, after the returned wait (at most PermitInterval, counted
// from when the claim was sent) when the permit is not due, after
// PermitInterval when another worker holds the channel's limiter row, after
// IdleWait when the channel has no due work, and, while the circuit breaker
// is open, when its cooldown ends, or after IdleWait once the cooldown is
// over and the probe is someone else's (K34).
// The loop always takes the channel whose next try comes first, so one
// channel's wait never delays another's claims. Beside the claim loop it runs
// the recovery step every RecoveryInterval (K40). When ctx ends it stops
// claiming and recovering at once, lets the calls in flight finish and write
// their results, and returns when they are done or ShutdownBudget has passed.
func (w *Worker) Run(ctx context.Context) error {
	slots := make(chan struct{}, w.cfg.InFlight)
	var calls, background sync.WaitGroup
	w.logger.Info("worker started", slog.Int("in_flight", w.cfg.InFlight),
		slog.String("lease", w.cfg.LeaseDuration.String()), slog.Int("max_attempts", w.cfg.MaxAttempts),
		slog.String("permit_interval", w.cfg.PermitInterval.String()),
		slog.String("permit_grace", w.cfg.PermitGrace.String()),
		slog.String("profile", string(w.cfg.Profile)),
		slog.Int("breaker_threshold", w.cfg.Breaker.Threshold),
		slog.String("breaker_cooldown", w.cfg.Breaker.Cooldown.String()),
		slog.String("breaker_max_cooldown", w.cfg.Breaker.MaxCooldown.String()))
	background.Go(func() { w.recoverLoop(ctx) })

	// nextTry holds each channel's next claim attempt; the zero time is now.
	nextTry := make(map[domain.Channel]time.Time, len(domain.Channels))
loop:
	for {
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			break loop
		}
		ch := earliest(nextTry)
		if wait := time.Until(nextTry[ch]); wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
			}
		}
		// select picks at random when two cases are ready, so the stop is
		// checked again: no claim after a stop.
		if ctx.Err() != nil {
			<-slots
			break loop
		}

		a, err := w.claim(ctx, ch)
		now := time.Now()
		if err != nil {
			<-slots
			w.logger.Error("claim failed", slog.String("channel", string(ch)), slog.String("error", err.Error()))
			nextTry[ch] = now.Add(w.cfg.IdleWait)
			continue
		}
		switch a.Outcome {
		case domain.ClaimClaimed:
			nextTry[ch] = now
			calls.Go(func() {
				defer func() { <-slots }()
				w.deliver(a.claimed)
			})
			continue
		case domain.ClaimThrottled:
			// The wait runs from the database's permit reading, which came
			// after t0, so t0 + wait is the earliest the permit can be due.
			nextTry[ch] = a.sent.Add(min(a.Wait, w.cfg.PermitInterval))
		case domain.ClaimBusy:
			nextTry[ch] = now.Add(w.cfg.PermitInterval)
		case domain.ClaimPaused:
			if a.Wait > 0 {
				// As for a throttled claim: the database read the cooldown's
				// end after t0.
				nextTry[ch] = a.sent.Add(a.Wait)
			} else {
				nextTry[ch] = now.Add(w.cfg.IdleWait)
			}
		default:
			nextTry[ch] = now.Add(w.cfg.IdleWait)
		}
		<-slots
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

// earliest returns the channel whose next try comes first; on a tie, the
// first in domain.Channels.
func earliest(nextTry map[domain.Channel]time.Time) domain.Channel {
	best := domain.Channels[0]
	for _, ch := range domain.Channels[1:] {
		if nextTry[ch].Before(nextTry[best]) {
			best = ch
		}
	}
	return best
}

// claim makes one claim attempt on ch. It reads the monotonic clock just
// before it sends the claim and sets the lease deadline and the start
// deadline from that reading (K3, K42). The claim statement is not cancelled
// by a stop: cancelling a statement that may already have committed would
// leave a lease nobody holds until recovery.
func (w *Worker) claim(ctx context.Context, ch domain.Channel) (attempt, error) {
	qctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), queryTimeout)
	defer cancel()

	t0 := time.Now()
	res, err := w.store.ClaimNext(qctx, domain.ClaimRequest{
		Channel:        ch,
		Lease:          w.cfg.LeaseDuration,
		MaxAttempts:    w.cfg.MaxAttempts,
		PermitInterval: w.cfg.PermitInterval,
		PermitGrace:    w.cfg.PermitGrace,
	})
	if err != nil {
		return attempt{}, err
	}
	a := attempt{ClaimResult: res, sent: t0}
	if res.Outcome == domain.ClaimClaimed {
		a.claimed = claimed{
			Claim:    res.Claim,
			deadline: t0.Add(res.Claim.LeaseRemaining),
			startBy:  t0.Add(res.Claim.PermitRemaining),
		}
	}
	return a, nil
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
	attrs := []any{
		slog.String("correlation_id", c.CorrelationID),
		slog.String("notification_id", c.ID),
		slog.String("channel", string(c.Channel)),
		slog.Int("attempt", c.Attempt),
	}
	if c.Probe {
		attrs = append(attrs, slog.Bool("breaker_probe", true))
	}
	log := w.logger.With(attrs...)
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
	// The start gate checks the permit's start deadline and, when it passes,
	// writes the start line with the send start it read (K21, K42).
	callCtx = provider.WithPermit(callCtx, provider.Permit{
		Deadline: c.startBy,
		OnStart: func(start time.Time) {
			log.Info("provider call started", slog.Time("started_at", start))
		},
	})

	if w.hooks.beforeGate != nil {
		w.hooks.beforeGate(c.Claim)
	}
	start := time.Now()
	resp, callErr := w.provider.Send(callCtx, provider.Request{
		To:            c.Recipient,
		Channel:       string(c.Channel),
		Content:       c.Content,
		CorrelationID: c.CorrelationID,
	})
	duration := time.Since(start)
	if errors.Is(callErr, provider.ErrStartDeadline) {
		w.giveBack(log, c)
		return
	}
	outcome := provider.Classify(resp, callErr, w.cfg.Profile)

	if w.hooks.beforeWrite != nil {
		w.hooks.beforeWrite(c.Claim)
	}

	// A new context for the write: the call's may have ended. It is bounded
	// by the lease, after which the write could not succeed anyway (K15).
	dbCtx, cancelDB := context.WithDeadline(context.Background(), c.deadline)
	defer cancelDB()

	result := domain.Result{Outage: outcome.Outage}
	if outcome.SharedQuota {
		// A shared quota's 429 pauses every channel until its Retry-After
		// (K34); without a valid one it delays only its notification.
		if pause, ok := provider.ParseRetryAfter(resp.RetryAfter, time.Now()); ok && pause > 0 {
			result.QuotaPause = pause
		}
	}
	callAttrs := []slog.Attr{
		slog.Int("status_code", resp.StatusCode),
		slog.Float64("duration_ms", float64(duration.Microseconds())/1000),
		slog.String("class", string(outcome.Class)),
	}
	var status domain.Status
	switch {
	case outcome.Class == provider.ClassAccepted:
		if resp.BodyProblem != "" {
			log.Warn("provider accepted the notification but its response body is not usable; provider fields left empty",
				slog.String("problem", resp.BodyProblem))
		}
		status = domain.StatusSent
		result.Kind = domain.ResultSent
		result.Acceptance = resp.Acceptance
	case outcome.Class == provider.ClassTemporary && c.Attempt < w.cfg.MaxAttempts:
		retryAfter, hasRetryAfter := provider.ParseRetryAfter(resp.RetryAfter, time.Now())
		wait := NextWait(c.Attempt, w.cfg.BackoffBase, w.cfg.BackoffCap, retryAfter, hasRetryAfter)
		status = domain.StatusQueued
		callAttrs = append(callAttrs, slog.Float64("retry_in_ms", float64(wait.Microseconds())/1000))
		result.Kind = domain.ResultRetry
		result.LastError = outcome.Error
		result.Wait = wait
	default:
		// Permanent, terminal (K41), or temporary on the last attempt.
		status = domain.StatusFailed
		result.Kind = domain.ResultFailed
		result.LastError = outcome.Error
	}
	fin, writeErr := w.store.Finish(dbCtx, domain.Finish{
		Attempt: domain.AttemptOf(c.Claim),
		Result:  result,
		Policy:  w.cfg.Breaker,
	})

	callAttrs = append(callAttrs, slog.String("outcome", string(status)))
	if outcome.Error != "" {
		callAttrs = append(callAttrs, slog.String("error", outcome.Error))
	}
	switch {
	case writeErr != nil && fin.Written:
		log.LogAttrs(dbCtx, slog.LevelError, "provider call finished, breaker not updated",
			append(callAttrs, slog.String("write_error", writeErr.Error()))...)
	case writeErr != nil:
		log.LogAttrs(dbCtx, slog.LevelError, "provider call finished, result not written",
			append(callAttrs, slog.String("write_error", writeErr.Error()))...)
	case !fin.Written:
		log.LogAttrs(dbCtx, slog.LevelWarn, "provider call finished, lease lost, result not written", callAttrs...)
	default:
		log.LogAttrs(dbCtx, slog.LevelInfo, "provider call finished", callAttrs...)
	}
	logBreaker(dbCtx, log, fin)
}

// logBreaker writes the breaker's change, if the result write made one (K21,
// K34).
func logBreaker(ctx context.Context, log *slog.Logger, fin domain.Finished) {
	attrs := []slog.Attr{
		slog.Int64("generation", fin.Generation),
		slog.String("cooldown", fin.Cooldown.String()),
	}
	switch fin.Breaker {
	case domain.BreakerOpened:
		log.LogAttrs(ctx, slog.LevelWarn, "breaker opened",
			append(attrs, slog.String("open_for", fin.OpenFor.Round(time.Millisecond).String()))...)
	case domain.BreakerReopened:
		log.LogAttrs(ctx, slog.LevelWarn, "breaker reopened",
			append(attrs, slog.String("open_for", fin.OpenFor.Round(time.Millisecond).String()))...)
	case domain.BreakerClosed:
		log.LogAttrs(ctx, slog.LevelInfo, "breaker closed", attrs...)
	}
}

// giveBack returns a claim whose send never started because its start
// deadline had passed (K42). No start line was written. The row goes back to
// queued with its attempt returned, once, under the lease and token check;
// the permit is not given back. A probe's give-back frees its slot (K34).
func (w *Worker) giveBack(log *slog.Logger, c claimed) {
	ctx, cancel := context.WithDeadline(context.Background(), c.deadline)
	defer cancel()
	late := slog.Float64("late_ms", float64(time.Since(c.startBy).Microseconds())/1000)
	given, err := w.store.GiveBack(ctx, domain.AttemptOf(c.Claim))
	switch {
	case err != nil:
		log.LogAttrs(ctx, slog.LevelError, "start deadline missed, no request sent, attempt not given back",
			late, slog.String("write_error", err.Error()))
	case !given:
		log.LogAttrs(ctx, slog.LevelWarn, "start deadline missed, no request sent, lease lost", late)
	default:
		log.LogAttrs(ctx, slog.LevelWarn, "start deadline missed, no request sent, notification given back", late)
	}
}
