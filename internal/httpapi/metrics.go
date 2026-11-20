package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// asOfLayout is RFC 3339 with microseconds, the database clock's precision.
const asOfLayout = "2006-01-02T15:04:05.000000Z07:00"

// MetricsBody is the response of GET /metrics (K20).
type MetricsBody struct {
	AsOf     string              `json:"as_of" format:"date-time" doc:"The database time every value refers to; RFC 3339 with microseconds"`
	Channels ChannelsMetricsBody `json:"channels"`
}

// ChannelsMetricsBody holds every channel.
type ChannelsMetricsBody struct {
	SMS   ChannelMetricsBody `json:"sms"`
	Email ChannelMetricsBody `json:"email"`
	Push  ChannelMetricsBody `json:"push"`
}

// ChannelMetricsBody is one channel's metrics.
type ChannelMetricsBody struct {
	Queue      QueueBody     `json:"queue"`
	Processing int           `json:"processing" minimum:"0" doc:"Notifications a worker holds at as_of; high while nothing completes means a stalled worker"`
	Completed  CompletedBody `json:"completed"`
	Latency    LatencyBody   `json:"latency" doc:"created_at → sent_at of the window's sent notifications that were not scheduled, queue waiting and retries included"`
	// K26: a planned wait is not delivery latency.
	ScheduledLatency LatencyBody `json:"scheduled_latency" doc:"scheduled_at → sent_at of the window's sent notifications that were scheduled"`
}

// QueueBody holds every priority.
type QueueBody struct {
	High   PriorityQueueBody `json:"high"`
	Normal PriorityQueueBody `json:"normal"`
	Low    PriorityQueueBody `json:"low"`
}

// PriorityQueueBody is the queued work of one channel and priority.
type PriorityQueueBody struct {
	Ready                 int      `json:"ready" minimum:"0" doc:"queued with available_at at or before as_of"`
	Deferred              int      `json:"deferred" minimum:"0" doc:"queued with a later available_at, such as a retry waiting for its time"`
	OldestReadyAgeSeconds *float64 `json:"oldest_ready_age_seconds" nullable:"true" doc:"as_of minus the earliest available_at of the ready notifications; null when none is ready"`
}

// CompletedBody counts the notifications that became sent or failed in the
// window.
type CompletedBody struct {
	WindowSeconds   int      `json:"window_seconds" doc:"Always 60: completed_at after as_of − 60 s and at or before as_of"`
	Sent            int      `json:"sent" minimum:"0"`
	Failed          int      `json:"failed" minimum:"0"`
	SentPerSecond   float64  `json:"sent_per_second" minimum:"0" doc:"sent / 60"`
	FailedPerSecond float64  `json:"failed_per_second" minimum:"0" doc:"failed / 60"`
	SuccessRatio    *float64 `json:"success_ratio" nullable:"true" minimum:"0" maximum:"1" doc:"sent / (sent + failed); null when nothing completed. Cancellations are not completions"`
}

// LatencyBody summarizes the time to sent_at of one group of the window's
// sent notifications.
type LatencyBody struct {
	Samples int      `json:"samples" minimum:"0" doc:"Notifications of this group sent in the window"`
	P50Ms   *float64 `json:"p50_ms" nullable:"true" doc:"null when samples is 0"`
	P95Ms   *float64 `json:"p95_ms" nullable:"true" doc:"null when samples is 0"`
	P99Ms   *float64 `json:"p99_ms" nullable:"true" doc:"null when samples is 0"`
}

type metricsOutput struct {
	Body MetricsBody
}

func registerMetrics(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "get-metrics",
		Method:      http.MethodGet,
		Path:        "/metrics",
		Summary:     "Queue depth, completions and latency per channel",
		Description: "Reads every value in one database snapshot taken at as_of: per channel and priority the queued " +
			"notifications that are ready and deferred and the age of the oldest ready one; the processing count; the " +
			"notifications sent and failed in the last 60 seconds with their rates and success ratio; and the p50, p95 " +
			"and p99 of created_at → sent_at for those sent without scheduled_at and of scheduled_at → sent_at for the " +
			"scheduled ones. It summarizes completed notifications; the rate limit is " +
			"proven by counting send starts, not by these numbers.",
		Tags: []string{"Metrics"},
	}, func(ctx context.Context, _ *struct{}) (*metricsOutput, error) {
		m, err := deps.Metrics.Snapshot(ctx)
		if err != nil {
			return nil, problemFor(ctx, loggerFrom(ctx, deps.Logger), err)
		}
		return &metricsOutput{Body: MetricsBody{
			AsOf: m.AsOf.UTC().Format(asOfLayout),
			Channels: ChannelsMetricsBody{
				SMS:   channelBody(m.Channels[domain.ChannelSMS]),
				Email: channelBody(m.Channels[domain.ChannelEmail]),
				Push:  channelBody(m.Channels[domain.ChannelPush]),
			},
		}}, nil
	})
}

func channelBody(c domain.ChannelMetrics) ChannelMetricsBody {
	return ChannelMetricsBody{
		Queue: QueueBody{
			High:   queueBody(c.Queue[domain.PriorityHigh]),
			Normal: queueBody(c.Queue[domain.PriorityNormal]),
			Low:    queueBody(c.Queue[domain.PriorityLow]),
		},
		Processing: c.Processing,
		Completed: CompletedBody{
			WindowSeconds:   int(domain.MetricsWindow / time.Second),
			Sent:            c.Completed.Sent,
			Failed:          c.Completed.Failed,
			SentPerSecond:   c.Completed.SentPerSecond(),
			FailedPerSecond: c.Completed.FailedPerSecond(),
			SuccessRatio:    c.Completed.SuccessRatio(),
		},
		Latency:          latencyBody(c.Latency),
		ScheduledLatency: latencyBody(c.ScheduledLatency),
	}
}

func latencyBody(l domain.Latency) LatencyBody {
	return LatencyBody{Samples: l.Samples, P50Ms: l.P50, P95Ms: l.P95, P99Ms: l.P99}
}

func queueBody(q domain.QueueDepth) PriorityQueueBody {
	b := PriorityQueueBody{Ready: q.Ready, Deferred: q.Deferred}
	if q.OldestReadyAge != nil {
		s := q.OldestReadyAge.Seconds()
		b.OldestReadyAgeSeconds = &s
	}
	return b
}
