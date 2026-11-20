package domain

import "time"

// MetricsWindow is the completion window of the metrics: a notification
// counts when its completed_at is in (as_of − 60 s, as_of] (K20).
const MetricsWindow = 60 * time.Second

// Priorities lists the three priorities, highest first.
var Priorities = []Priority{PriorityHigh, PriorityNormal, PriorityLow}

// Metrics is one snapshot of the queue and of the last window's completions,
// per channel (K20). Every value was read in one database snapshot taken at
// AsOf.
type Metrics struct {
	AsOf     time.Time
	Channels map[Channel]ChannelMetrics
}

// ChannelMetrics is one channel's part of a snapshot.
type ChannelMetrics struct {
	// Queue holds every priority.
	Queue map[Priority]QueueDepth
	// Processing counts the notifications a worker holds.
	Processing int
	Completed  Completions
	// Latency covers the notifications sent without scheduled_at, from
	// created_at; ScheduledLatency the scheduled ones, from scheduled_at, so a
	// planned wait never counts as delivery latency (K26).
	Latency          Latency
	ScheduledLatency Latency
}

// QueueDepth counts the queued notifications of one channel and priority.
type QueueDepth struct {
	// Ready: available_at at or before as_of.
	Ready int
	// Deferred: waiting for a later available_at, such as a retry.
	Deferred int
	// OldestReadyAge is as_of − the earliest available_at of the ready
	// notifications, or nil when none is ready; the starvation signal (K8).
	OldestReadyAge *time.Duration
}

// Completions counts the notifications that became sent or failed in the
// window. Cancellations and waiting retries are not completions.
type Completions struct {
	Sent   int
	Failed int
}

// SentPerSecond is Sent over the window; 0 when nothing was sent.
func (c Completions) SentPerSecond() float64 {
	return float64(c.Sent) / MetricsWindow.Seconds()
}

// FailedPerSecond is Failed over the window; 0 when nothing failed.
func (c Completions) FailedPerSecond() float64 {
	return float64(c.Failed) / MetricsWindow.Seconds()
}

// SuccessRatio is Sent / (Sent + Failed), or nil when nothing completed.
func (c Completions) SuccessRatio() *float64 {
	if c.Sent+c.Failed == 0 {
		return nil
	}
	r := float64(c.Sent) / float64(c.Sent+c.Failed)
	return &r
}

// Latency summarizes the time to sent_at of the notifications sent in the
// window, in milliseconds. The percentiles are nil when Samples is 0.
type Latency struct {
	Samples       int
	P50, P95, P99 *float64
}
