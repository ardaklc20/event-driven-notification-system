package httpapi_test

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func scheduledBody(at time.Time) string {
	return fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":"later","scheduled_at":%q}`,
		at.UTC().Format(time.RFC3339Nano))
}

// TestScheduledPastTime: a new request whose scheduled_at is one second in
// the past gets 422 naming body.scheduled_at; nothing is saved and the key
// stays free. A time without an offset and a past item in a batch get 422
// too (K26).
func TestScheduledPastTime(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	resp, body := post(t, base, scheduledBody(time.Now().Add(-time.Second)), withKey("past-1"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("body.scheduled_at") {
		t.Fatalf("past time: status %d body %s, want 422 naming body.scheduled_at", resp.StatusCode, body)
	}
	if n, k := rows(t, pool, "notifications"), rows(t, pool, "idempotency_keys"); n != 0 || k != 0 {
		t.Fatalf("%d notifications and %d keys after the 422, want none", n, k)
	}
	// The key was not used: the same key with a future time is accepted.
	if resp, body := post(t, base, scheduledBody(time.Now().Add(time.Hour)), withKey("past-1")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("same key, future time: status %d body %s, want 202", resp.StatusCode, body)
	}

	resp, body = post(t, base, `{"recipient":"+905551234567","channel":"sms","content":"x","scheduled_at":"2026-11-22T09:00:00"}`, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("body.scheduled_at") {
		t.Fatalf("no offset: status %d body %s, want 422 naming body.scheduled_at", resp.StatusCode, body)
	}

	batch := fmt.Sprintf(`{"notifications":[%s,%s]}`,
		scheduledBody(time.Now().Add(time.Hour)), scheduledBody(time.Now().Add(-time.Second)))
	resp, body = postBatch(t, base, []byte(batch), nil)
	if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("body.notifications[1].scheduled_at") {
		t.Fatalf("batch with a past item: status %d body %s, want 422 naming item 1", resp.StatusCode, body)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Fatalf("%d notifications, want only the earlier accepted one", n)
	}
}

// TestScheduledRepeatAfterTime: a request scheduled 2 seconds ahead with a
// key is repeated 3 seconds later, after its time. The past-time rule runs
// only for a new key, so the repeat gets the first response's bytes and no
// new row (K10, K26).
func TestScheduledRepeatAfterTime(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	req := scheduledBody(time.Now().Add(2 * time.Second))

	resp, first := post(t, base, req, withKey("later-1"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first: status %d body %s", resp.StatusCode, first)
	}
	time.Sleep(3 * time.Second)
	resp, again := post(t, base, req, withKey("later-1"))
	if resp.StatusCode != http.StatusAccepted || !bytes.Equal(first, again) {
		t.Fatalf("repeat: status %d body %s, want 202 with %s", resp.StatusCode, again, first)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Fatalf("%d notifications, want 1", n)
	}
}

// TestScheduledKeyConflict: the same key with a different scheduled_at is a
// different request: 422 naming the header, and no second row (K10, K26).
func TestScheduledKeyConflict(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	at := time.Now().Add(time.Hour)
	if resp, body := post(t, base, scheduledBody(at), withKey("sched-1")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first: status %d body %s", resp.StatusCode, body)
	}
	resp, body := post(t, base, scheduledBody(at.Add(time.Minute)), withKey("sched-1"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("header.Idempotency-Key") {
		t.Fatalf("different time: status %d body %s, want 422 naming the header", resp.StatusCode, body)
	}
	// The same instant written with another offset is the same request.
	same := fmt.Sprintf(`{"recipient":"+905551234567","channel":"sms","content":"later","scheduled_at":%q}`,
		at.In(time.FixedZone("TRT", 3*3600)).Format(time.RFC3339Nano))
	if resp, body := post(t, base, same, withKey("sched-1")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("same instant, +03:00: status %d body %s, want the replayed 202", resp.StatusCode, body)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Fatalf("%d notifications, want 1", n)
	}
}
