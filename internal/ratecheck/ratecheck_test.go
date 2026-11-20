package ratecheck_test

import (
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/ratecheck"
)

var base = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

// at returns base plus each offset in microseconds.
func at(us ...int) []time.Time {
	out := make([]time.Time, len(us))
	for i, u := range us {
		out[i] = base.Add(time.Duration(u) * time.Microsecond)
	}
	return out
}

// spaced returns n times step apart, starting at base + offset.
func spaced(n int, offset, step time.Duration) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = base.Add(offset + time.Duration(i)*step)
	}
	return out
}

// TestWindowCount checks the half-open window [t, t + 1 s) on hand-made start
// times (K43).
func TestWindowCount(t *testing.T) {
	for name, tt := range map[string]struct {
		times []time.Time
		want  int
	}{
		"none": {nil, 0},
		"one":  {at(0), 1},
		// 99, 100 and 101 starts packed into less than a second.
		"99 starts":  {spaced(99, 0, 10*time.Millisecond), 99},
		"100 starts": {spaced(100, 0, 10*time.Millisecond), 100},
		"101 starts": {spaced(101, 0, 9*time.Millisecond), 101},
		// 101 starts exactly 10 ms apart: the first and the last are exactly
		// 1 s apart and fall in different windows.
		"101 starts, last on the edge": {spaced(101, 0, 10*time.Millisecond), 100},
		"exactly one second apart":     {at(0, 1_000_000), 1},
		"just under one second":        {at(0, 999_999), 2},
		"edge excluded":                {at(0, 999_000, 1_000_000), 2},
		"later window wins":            {at(0, 1_500_000, 1_600_000, 1_700_000, 2_499_999), 4},
		"unsorted input":               {at(1_700_000, 0, 1_500_000, 2_499_999, 1_600_000), 4},
		// The K42 spacing: permits 11 ms apart, each start up to 100 ms late.
		"K42 worst case": {append(at(100_000), spaced(100, 1_100*time.Millisecond, 11*time.Millisecond)[1:]...), 91},
	} {
		if got := ratecheck.MaxInWindow(tt.times, time.Second); got != tt.want {
			t.Errorf("%s: %d, want %d", name, got, tt.want)
		}
	}

	starts := []ratecheck.Start{
		{Channel: "sms", WorkerID: "b", At: base},
		{Channel: "sms", WorkerID: "a", At: base.Add(time.Second)},
		{Channel: "email", WorkerID: "a", At: base},
		{Channel: "email", WorkerID: "a", At: base.Add(500 * time.Millisecond)},
	}
	got := ratecheck.PerChannel(starts)
	want := []ratecheck.Report{{"email", 2, 2}, {"sms", 2, 1}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("PerChannel = %+v, want %+v", got, want)
	}
	if w := ratecheck.Workers(starts); len(w) != 2 || w[0] != "a" || w[1] != "b" {
		t.Errorf("Workers = %v, want [a b]", w)
	}
}
