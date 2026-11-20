package mockprovider

import (
	"testing"
	"time"
)

// TestMaxInWindow checks the half-open window [t, t + 1 s): an arrival exactly
// one second after another falls in the next window.
func TestMaxInWindow(t *testing.T) {
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	at := func(ms ...int) []time.Time {
		out := make([]time.Time, len(ms))
		for i, m := range ms {
			out[i] = base.Add(time.Duration(m) * time.Millisecond)
		}
		return out
	}
	for name, tt := range map[string]struct {
		times []time.Time
		want  int
	}{
		"none":              {nil, 0},
		"one":               {at(0), 1},
		"edge excluded":     {at(0, 999, 1000), 2},
		"later window wins": {at(0, 1500, 1600, 1700, 2499), 4},
		"all in one second": {at(0, 100, 200, 300), 4},
	} {
		if got := maxInWindow(tt.times, time.Second); got != tt.want {
			t.Errorf("%s: %d, want %d", name, got, tt.want)
		}
	}
}
