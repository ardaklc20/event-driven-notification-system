package worker

import (
	"math/rand/v2"
	"time"
)

// Backoff returns the wait before the n-th retry (n ≥ 1): a random time in
// [0, min(capDur, base × 2^(n−1))], full jitter (K7).
func Backoff(n int, base, capDur time.Duration) time.Duration {
	bound := capDur
	if n >= 1 && n <= 62 && base <= capDur>>(n-1) {
		bound = base << (n - 1)
	}
	if bound <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(bound) + 1))
}

// NextWait returns the wait before the n-th retry: the backoff, or a valid
// Retry-After when it asks for longer, even beyond the cap (K7).
func NextWait(n int, base, capDur, retryAfter time.Duration, hasRetryAfter bool) time.Duration {
	wait := Backoff(n, base, capDur)
	if hasRetryAfter && retryAfter > wait {
		return retryAfter
	}
	return wait
}
