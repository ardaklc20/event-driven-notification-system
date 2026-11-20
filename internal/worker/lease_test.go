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

// unusedStore fails every call; fakes embed it and override what they use.
type unusedStore struct{}

var errNotUsed = errors.New("not used")

func (unusedStore) ClaimNext(context.Context, domain.ClaimRequest) (domain.ClaimResult, error) {
	return domain.ClaimResult{}, errNotUsed
}

func (unusedStore) Finish(context.Context, domain.Finish) (domain.Finished, error) {
	return domain.Finished{}, errNotUsed
}

func (unusedStore) GiveBack(context.Context, domain.Attempt) (bool, error) {
	return false, errNotUsed
}

func (unusedStore) RecoverExpired(context.Context, int, int) ([]domain.Recovered, error) {
	return nil, errNotUsed
}

// slowClaimer answers a claim after delay with fixed remaining times and
// records when it was called and when it answered.
type slowClaimer struct {
	unusedStore
	delay      time.Duration
	lease      time.Duration
	permit     time.Duration
	calledAt   time.Time
	answeredAt time.Time
}

func (s *slowClaimer) ClaimNext(context.Context, domain.ClaimRequest) (domain.ClaimResult, error) {
	s.calledAt = time.Now()
	time.Sleep(s.delay)
	s.answeredAt = time.Now()
	return domain.ClaimResult{Outcome: domain.ClaimClaimed, Claim: domain.Claim{
		ID: "n-1", LeaseToken: "token", Attempt: 1, LeaseRemaining: s.lease, PermitRemaining: s.permit,
	}}, nil
}

// TestLeaseDeadlineFromClaimStart: the claim takes 1 second and returns a
// remaining lease of 30 seconds and a remaining permit of 100 ms. Both
// deadlines must come from the time read before the claim was sent, never
// from the time its answer arrived (K3, K42). synctest's clock moves only when
// every goroutine waits, so the times are exact.
func TestLeaseDeadlineFromClaimStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const lease, permit = 30 * time.Second, 100 * time.Millisecond
		store := &slowClaimer{delay: time.Second, lease: lease, permit: permit}
		w := New(store, nil, slog.New(slog.DiscardHandler), Config{LeaseDuration: lease, MaxAttempts: 5})

		a, err := w.claim(t.Context(), domain.ChannelSMS)
		if err != nil || a.Outcome != domain.ClaimClaimed {
			t.Fatalf("claim: %+v err=%v", a, err)
		}
		c := a.claimed
		if !c.deadline.Equal(store.calledAt.Add(lease)) {
			t.Errorf("lease deadline %s, want t0 + 30 s = %s", c.deadline, store.calledAt.Add(lease))
		}
		if !c.startBy.Equal(store.calledAt.Add(permit)) {
			t.Errorf("start deadline %s, want t0 + 100 ms = %s", c.startBy, store.calledAt.Add(permit))
		}
		if !c.startBy.Before(store.answeredAt) {
			t.Errorf("start deadline %s is not before the answer's arrival %s: the 1 s claim used up the permit",
				c.startBy, store.answeredAt)
		}
		t.Logf("t0=%s answer=%s lease deadline=%s start deadline=%s", store.calledAt.Format(time.TimeOnly),
			store.answeredAt.Format(time.TimeOnly), c.deadline.Format(time.TimeOnly), c.startBy.Format(time.StampMilli))
	})
}
