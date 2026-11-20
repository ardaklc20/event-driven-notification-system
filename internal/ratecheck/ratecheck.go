// Package ratecheck counts events in half-open one-second windows, the way
// R8 is measured: the largest number of send starts per channel in any window
// [t, t + 1 s), with t running over every start time (K5, K43). The tests,
// cmd/ratecount and the mock provider's arrival counts all use it.
package ratecheck

import (
	"slices"
	"time"
)

// Window is the width of the R8 window.
const Window = time.Second

// MaxInWindow returns the largest number of times that fall in one half-open
// window [t, t + width). A window holding the most times can start at one of
// them, so only those starts are tried. times need not be sorted.
func MaxInWindow(times []time.Time, width time.Duration) int {
	sorted := slices.Clone(times)
	slices.SortFunc(sorted, func(a, b time.Time) int { return a.Compare(b) })
	best, end := 0, 0
	for start := range sorted {
		for end < len(sorted) && sorted[end].Sub(sorted[start]) < width {
			end++
		}
		best = max(best, end-start)
	}
	return best
}

// Start is one send start, as the worker's start line records it.
type Start struct {
	Channel  string
	WorkerID string
	At       time.Time
}

// Report is the count for one channel.
type Report struct {
	Channel string
	// Starts is the number of send starts on the channel.
	Starts int
	// MaxWindow is the largest number of them in one half-open window.
	MaxWindow int
}

// PerChannel counts starts per channel, sorted by channel name.
func PerChannel(starts []Start) []Report {
	byChannel := map[string][]time.Time{}
	for _, s := range starts {
		byChannel[s.Channel] = append(byChannel[s.Channel], s.At)
	}
	out := make([]Report, 0, len(byChannel))
	for ch, times := range byChannel {
		out = append(out, Report{Channel: ch, Starts: len(times), MaxWindow: MaxInWindow(times, Window)})
	}
	slices.SortFunc(out, func(a, b Report) int {
		switch {
		case a.Channel < b.Channel:
			return -1
		case a.Channel > b.Channel:
			return 1
		}
		return 0
	})
	return out
}

// Workers returns the distinct worker IDs among starts, sorted.
func Workers(starts []Start) []string {
	var ids []string
	for _, s := range starts {
		if !slices.Contains(ids, s.WorkerID) {
			ids = append(ids, s.WorkerID)
		}
	}
	slices.Sort(ids)
	return ids
}
