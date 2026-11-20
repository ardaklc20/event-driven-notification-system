package worker

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// call is one claim attempt the scripted claimer answered.
type call struct {
	at      time.Time
	outcome domain.ClaimResult
}

// scriptedClaimer answers the i-th claim on a channel with script[ch](i) and
// records every call. A claimed notification has no lease left, so the
// worker sends nothing and the slot comes back at once.
type scriptedClaimer struct {
	unusedStore
	script map[domain.Channel]func(i int) domain.ClaimResult

	mu    sync.Mutex
	calls map[domain.Channel][]call
}

func (s *scriptedClaimer) ClaimNext(_ context.Context, req domain.ClaimRequest) (domain.ClaimResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := s.script[req.Channel](len(s.calls[req.Channel]))
	s.calls[req.Channel] = append(s.calls[req.Channel], call{at: time.Now(), outcome: res})
	return res, nil
}

func (*scriptedClaimer) RecoverExpired(context.Context, int, int) ([]domain.Recovered, error) {
	return nil, nil
}

var (
	claimedNow = domain.ClaimResult{Outcome: domain.ClaimClaimed, Claim: domain.Claim{ID: "n", LeaseToken: "t"}}
	busy       = domain.ClaimResult{Outcome: domain.ClaimBusy}
	empty      = domain.ClaimResult{Outcome: domain.ClaimEmpty}
)

func throttled(wait time.Duration) domain.ClaimResult {
	return domain.ClaimResult{Outcome: domain.ClaimThrottled, Wait: wait}
}

// sequence answers with results in order, then repeats the last one.
func sequence(results ...domain.ClaimResult) func(int) domain.ClaimResult {
	return func(i int) domain.ClaimResult { return results[min(i, len(results)-1)] }
}

// cycle answers with results in order, again and again.
func cycle(results ...domain.ClaimResult) func(int) domain.ClaimResult {
	return func(i int) domain.ClaimResult { return results[i%len(results)] }
}

// TestClaimLoopWaits: each outcome sets its channel's next try, and a channel
// that waits never delays another's claims (K5). synctest's clock moves only
// when every goroutine waits, so the gaps are exact.
func TestClaimLoopWaits(t *testing.T) {
	const (
		permitInterval = 11 * time.Millisecond
		idleWait       = 200 * time.Millisecond
	)
	// want is the gap after a call with this outcome.
	want := func(r domain.ClaimResult) time.Duration {
		switch r.Outcome {
		case domain.ClaimClaimed:
			return 0
		case domain.ClaimThrottled:
			return min(r.Wait, permitInterval)
		case domain.ClaimBusy:
			return permitInterval
		default:
			return idleWait
		}
	}

	for _, tt := range []struct {
		name   string
		script map[domain.Channel]func(int) domain.ClaimResult
		// first are the gaps the sms channel must show first, spelled out.
		first []time.Duration
	}{
		{
			name: "each outcome on sms",
			script: map[domain.Channel]func(int) domain.ClaimResult{
				domain.ChannelSMS: sequence(claimedNow, throttled(7*time.Millisecond), busy,
					throttled(time.Minute), empty),
				domain.ChannelEmail: sequence(empty),
				domain.ChannelPush:  sequence(empty),
			},
			// After claimed: at once; throttled 7 ms: 7 ms; busy: 11 ms;
			// throttled 1 min: capped at 11 ms; empty: 200 ms.
			first: []time.Duration{0, 7 * time.Millisecond, permitInterval, permitInterval, idleWait},
		},
		{
			name: "empty sms does not delay email",
			script: map[domain.Channel]func(int) domain.ClaimResult{
				domain.ChannelSMS:   sequence(empty),
				domain.ChannelEmail: cycle(claimedNow, claimedNow, throttled(5*time.Millisecond)),
				domain.ChannelPush:  sequence(busy),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := &scriptedClaimer{script: tt.script, calls: map[domain.Channel][]call{}}
				w := New(store, nil, slog.New(slog.DiscardHandler), Config{
					InFlight: 2, IdleWait: idleWait, ShutdownBudget: time.Second, MaxAttempts: 5,
					RecoveryInterval: time.Hour, RecoveryBatch: 1, PermitInterval: permitInterval,
				})
				ctx, cancel := context.WithCancel(t.Context())
				done := make(chan error, 1)
				go func() { done <- w.Run(ctx) }()
				time.Sleep(time.Second)
				cancel()
				if err := <-done; err != nil {
					t.Fatalf("Run: %v", err)
				}

				store.mu.Lock()
				defer store.mu.Unlock()
				for ch, calls := range store.calls {
					if len(calls) < 5 {
						t.Fatalf("%s: %d claim attempts in one second, want at least 5", ch, len(calls))
					}
					for i := 1; i < len(calls); i++ {
						gap := calls[i].at.Sub(calls[i-1].at)
						if gap != want(calls[i-1].outcome) {
							t.Fatalf("%s: attempt %d came %s after a %s attempt, want %s",
								ch, i, gap, calls[i-1].outcome.Outcome, want(calls[i-1].outcome))
						}
					}
				}
				for i, gap := range tt.first {
					if got := store.calls[domain.ChannelSMS][i+1].at.Sub(store.calls[domain.ChannelSMS][i].at); got != gap {
						t.Errorf("sms gap %d = %s, want %s", i, got, gap)
					}
				}
				t.Logf("attempts in one second: sms %d, email %d, push %d", len(store.calls[domain.ChannelSMS]),
					len(store.calls[domain.ChannelEmail]), len(store.calls[domain.ChannelPush]))
			})
		})
	}
}
