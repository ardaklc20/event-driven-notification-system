package worker_test

import (
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

// TestBackoff: the n-th retry waits within [0, min(60 s, 2 s × 2^(n−1))], the
// waits spread across that range, and a longer Retry-After wins (K7).
func TestBackoff(t *testing.T) {
	const (
		base    = 2 * time.Second
		capDur  = 60 * time.Second
		samples = 2000
	)
	for n, bound := range map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second, 4: 16 * time.Second, 6: 60 * time.Second, 100: 60 * time.Second} {
		var low, high bool
		for range samples {
			w := worker.Backoff(n, base, capDur)
			if w < 0 || w > bound {
				t.Fatalf("retry %d: wait %s outside [0, %s]", n, w, bound)
			}
			low = low || w < bound/4
			high = high || w > bound*3/4
		}
		if !low || !high {
			t.Errorf("retry %d: %d waits did not spread over [0, %s] (low %v, high %v)", n, samples, bound, low, high)
		}
	}

	if w := worker.Backoff(3, 0, 0); w != 0 {
		t.Errorf("zero base: wait %s, want 0", w)
	}

	// A Retry-After longer than any backoff wins, beyond the 60-second cap.
	if w := worker.NextWait(1, base, capDur, 90*time.Second, true); w != 90*time.Second {
		t.Errorf("Retry-After 90s: wait %s, want 90s", w)
	}
	// A shorter one never shortens the backoff below its own draw, and an
	// invalid one is ignored.
	for range samples {
		if w := worker.NextWait(4, base, capDur, time.Second, true); w < time.Second || w > 16*time.Second {
			t.Fatalf("Retry-After 1s on retry 4: wait %s outside [1s, 16s]", w)
		}
		if w := worker.NextWait(1, base, capDur, time.Hour, false); w > 2*time.Second {
			t.Fatalf("invalid Retry-After was used: wait %s", w)
		}
	}
}
