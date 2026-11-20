package httpapi_test

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// TestMetricsOperation: GET /metrics answers 200 with every field for sms,
// email and push, null where nothing was measured, and the operation is in
// the OpenAPI document (R14, K19, K20).
func TestMetricsOperation(t *testing.T) {
	pool := testdb.New(t)
	store := postgres.NewStore(pool)
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Logger:        slog.New(slog.DiscardHandler),
		Readiness:     &httpapi.Readiness{},
		Notifications: service.NewNotifications(store, domain.DefaultLimits()),
		Metrics:       service.NewMetrics(store),
	}))
	t.Cleanup(srv.Close)
	// Small fixture: one ready sms, one sent sms 2 s after its creation.
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO notifications (recipient, channel, content, priority, correlation_id, status,
		                           available_at, created_at, sent_at, completed_at)
		VALUES ('+905551234567', 'sms', 'a', 'high', 'c', 'queued', now() - interval '1 second', now(), NULL, NULL),
		       ('+905551234567', 'sms', 'b', 'normal', 'c', 'sent', now(), clock_timestamp() - interval '3 seconds',
		        clock_timestamp() - interval '1 second', clock_timestamp() - interval '1 second')`); err != nil {
		t.Fatal(err)
	}

	resp, raw := do(t, http.MethodGet, srv.URL+"/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	var body struct {
		AsOf     string                    `json:"as_of"`
		Channels map[string]map[string]any `json:"channels"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if _, err := time.Parse(time.RFC3339Nano, body.AsOf); err != nil || len(body.AsOf) != len("2006-01-02T15:04:05.000000Z") {
		t.Errorf("as_of %q, want RFC 3339 in UTC with microseconds", body.AsOf)
	}
	for _, ch := range []string{"sms", "email", "push"} {
		c := body.Channels[ch]
		queue, _ := c["queue"].(map[string]any)
		for _, p := range []string{"high", "normal", "low"} {
			q, _ := queue[p].(map[string]any)
			for _, f := range []string{"ready", "deferred", "oldest_ready_age_seconds"} {
				if _, ok := q[f]; !ok {
					t.Errorf("%s.queue.%s.%s missing", ch, p, f)
				}
			}
		}
		if _, ok := c["processing"]; !ok {
			t.Errorf("%s.processing missing", ch)
		}
		completed, _ := c["completed"].(map[string]any)
		for _, f := range []string{"window_seconds", "sent", "failed", "sent_per_second", "failed_per_second", "success_ratio"} {
			if _, ok := completed[f]; !ok {
				t.Errorf("%s.completed.%s missing", ch, f)
			}
		}
		latency, _ := c["latency"].(map[string]any)
		for _, f := range []string{"samples", "p50_ms", "p95_ms", "p99_ms"} {
			if _, ok := latency[f]; !ok {
				t.Errorf("%s.latency.%s missing", ch, f)
			}
		}
	}

	var typed httpapi.MetricsBody
	if err := json.Unmarshal(raw, &typed); err != nil {
		t.Fatal(err)
	}
	sms := typed.Channels.SMS
	if sms.Queue.High.Ready != 1 || sms.Queue.High.OldestReadyAgeSeconds == nil || sms.Completed.Sent != 1 ||
		sms.Completed.WindowSeconds != 60 || sms.Completed.SuccessRatio == nil || *sms.Completed.SuccessRatio != 1 ||
		sms.Latency.Samples != 1 || sms.Latency.P50Ms == nil || *sms.Latency.P50Ms < 1999 || *sms.Latency.P50Ms > 2001 {
		t.Errorf("sms = %+v, want 1 ready high with an age, 1 sent, ratio 1, p50 2000 ms", sms)
	}
	email := typed.Channels.Email
	if email.Completed.SuccessRatio != nil || email.Latency.P50Ms != nil || email.Queue.Low.OldestReadyAgeSeconds != nil {
		t.Errorf("email = %+v, want nulls where nothing was measured", email)
	}

	doc, err := httpapi.OpenAPIYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "/metrics:") || !strings.Contains(string(doc), "operationId: get-metrics") {
		t.Error("GET /metrics is not in the OpenAPI document")
	}
}
