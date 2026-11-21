package worker_test

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

// An accepted delivery must not be retried because an optional provider field
// cannot be stored in PostgreSQL text. Keep the valid timestamp, discard the
// unusable message ID and warn once.
func TestAcceptedWithNULMessageID(t *testing.T) {
	pool := testdb.New(t)
	id := insertQueued(t, pool, 1, "corr-nul-message-id")[0]
	var calls atomic.Int32
	rec, srv := newProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			accepted(`{"messageId":"m-\u0000123","timestamp":"2026-11-20T10:30:00Z"}`)(w, r)
			return
		}
		// A regression can complete its unwanted retry, so it fails with the
		// actual number of POSTs instead of waiting out every attempt.
		accepted(validAccepted)(w, r)
	})

	s := defaults()
	s.cfg.LeaseDuration = time.Second
	s.cfg.ResultBudget = 300 * time.Millisecond
	s.httpTimeout = 200 * time.Millisecond
	w := startWorker(t, pool, srv.URL+"/", s)
	r := waitStatus(t, pool, id, "sent", 10*time.Second)
	if err := w.stop(t); err != nil {
		t.Fatalf("worker: %v", err)
	}

	if n := len(rec.all()); n != 1 {
		t.Fatalf("provider got %d POSTs after accepting the first, want 1", n)
	}
	wantTimestamp := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	if r.attempts != 1 || r.token != nil || r.expires != nil || r.sentAt == nil || r.messageID != nil ||
		r.timestamp == nil || !r.timestamp.Equal(wantTimestamp) {
		t.Errorf("row = %+v, want one sent attempt, no lease or message ID, and the valid provider timestamp", r)
	}
	warnings := countLevel(w.logs.records(t), "WARN")
	if len(warnings) != 1 || warnings[0]["correlation_id"] != "corr-nul-message-id" ||
		warnings[0]["notification_id"] != id || warnings[0]["problem"] != "messageId contains NUL" {
		t.Errorf("warnings = %v, want one messageId NUL warning carrying the notification and correlation IDs", warnings)
	}
}
