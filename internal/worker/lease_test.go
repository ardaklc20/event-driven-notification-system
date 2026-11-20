package worker

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// slowClaimer answers a claim after delay with a fixed remaining lease and
// records when it was called and when it answered.
type slowClaimer struct {
	delay      time.Duration
	remaining  time.Duration
	calledAt   time.Time
	answeredAt time.Time
}

func (s *slowClaimer) ClaimNext(context.Context, time.Duration) (domain.Claim, bool, error) {
	s.calledAt = time.Now()
	time.Sleep(s.delay)
	s.answeredAt = time.Now()
	return domain.Claim{ID: "n-1", LeaseToken: "token", Attempt: 1, LeaseRemaining: s.remaining}, true, nil
}

func (s *slowClaimer) MarkSent(context.Context, string, string, domain.Acceptance) (bool, error) {
	return false, errors.New("not used")
}

func (s *slowClaimer) MarkFailed(context.Context, string, string, string) (bool, error) {
	return false, errors.New("not used")
}

// TestLeaseDeadlineFromClaimStart: the claim takes 1 second and returns a
// remaining lease of 30 seconds. The deadline must come from the time read
// before the claim was sent, never from the time its answer arrived (K3).
// synctest's clock moves only when every goroutine waits, so the times are
// exact.
func TestLeaseDeadlineFromClaimStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const remaining = 30 * time.Second
		store := &slowClaimer{delay: time.Second, remaining: remaining}
		w := New(store, nil, slog.New(slog.DiscardHandler), Config{LeaseDuration: remaining})

		c, ok, err := w.claim(t.Context())
		if err != nil || !ok {
			t.Fatalf("claim: ok=%v err=%v", ok, err)
		}
		if c.deadline.After(store.calledAt.Add(remaining)) {
			t.Errorf("deadline %s is after t0 + 30 s = %s", c.deadline, store.calledAt.Add(remaining))
		}
		if latest := store.answeredAt.Add(remaining - time.Second); c.deadline.After(latest) {
			t.Errorf("deadline %s is later than the answer's arrival + 30 s − 1 s = %s", c.deadline, latest)
		}
		t.Logf("t0=%s answer=%s deadline=%s", store.calledAt.Format(time.TimeOnly),
			store.answeredAt.Format(time.TimeOnly), c.deadline.Format(time.TimeOnly))
	})
}
