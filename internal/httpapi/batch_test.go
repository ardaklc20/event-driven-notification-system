package httpapi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

func batchServer(t *testing.T, pool *pgxpool.Pool, limits domain.Limits, maxBody int64) string {
	t.Helper()
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Logger:            slog.New(slog.DiscardHandler),
		Readiness:         &httpapi.Readiness{},
		Notifications:     service.NewNotifications(postgres.NewStore(pool), limits),
		BatchMaxBodyBytes: maxBody,
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// mixedItems returns n valid items cycling through the channels and
// priorities, each with its index in the content.
func mixedItems(n int) []httpapi.NotificationRequest {
	recipients := []string{"+905551234567", "user@example.com", "device-token-1"}
	channels := []string{"sms", "email", "push"}
	priorities := []string{"high", "normal", "low", ""}
	items := make([]httpapi.NotificationRequest, n)
	for i := range items {
		items[i] = httpapi.NotificationRequest{
			Recipient: recipients[i%3],
			Channel:   channels[i%3],
			Content:   fmt.Sprintf("item %d", i),
			Priority:  priorities[i%4],
		}
	}
	return items
}

func batchJSON(t *testing.T, items []httpapi.NotificationRequest) []byte {
	t.Helper()
	b, err := json.Marshal(httpapi.BatchRequest{Notifications: items})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func postBatch(t *testing.T, base string, body []byte, header http.Header) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+"/notifications/batch", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST batch: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func decodeBatchAccepted(t *testing.T, body []byte) httpapi.BatchAcceptedBody {
	t.Helper()
	var a httpapi.BatchAcceptedBody
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return a
}

// nothingWritten fails the test when a batch, a notification or a key exists.
func nothingWritten(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"batches", "notifications", "idempotency_keys"} {
		if n := rows(t, pool, table); n != 0 {
			t.Errorf("%s has %d rows, want 0", table, n)
		}
	}
}

// TestBatchSizeLimits: 1,000 valid mixed items are saved, each with the batch
// ID; 0 and 1,001 items get 422 and write nothing (R2, K12).
func TestBatchSizeLimits(t *testing.T) {
	t.Run("1000 items", func(t *testing.T) {
		pool := testdb.New(t)
		base := batchServer(t, pool, domain.DefaultLimits(), 0)
		body := batchJSON(t, mixedItems(domain.MaxBatchItems))
		start := time.Now()
		resp, raw := postBatch(t, base, body, nil)
		elapsed := time.Since(start)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status %d, want 202: %s", resp.StatusCode, raw)
		}
		a := decodeBatchAccepted(t, raw)
		if len(a.NotificationIDs) != domain.MaxBatchItems {
			t.Fatalf("%d IDs, want 1000", len(a.NotificationIDs))
		}
		var saved, withBatch, channels, priorities int
		if err := pool.QueryRow(t.Context(),
			`SELECT count(*), count(*) FILTER (WHERE batch_id = $1::text::uuid),
			        count(DISTINCT channel), count(DISTINCT priority)
			 FROM notifications`, a.BatchID).Scan(&saved, &withBatch, &channels, &priorities); err != nil {
			t.Fatal(err)
		}
		if saved != 1000 || withBatch != 1000 || channels != 3 || priorities != 3 {
			t.Errorf("saved %d, with the batch ID %d, channels %d, priorities %d; want 1000, 1000, 3, 3",
				saved, withBatch, channels, priorities)
		}
		t.Logf("1000-item batch (%d bytes) answered 202 in %s", len(body), elapsed.Round(time.Millisecond))
	})

	for name, n := range map[string]int{"0 items": 0, "1001 items": domain.MaxBatchItems + 1} {
		t.Run(name, func(t *testing.T) {
			pool := testdb.New(t)
			base := batchServer(t, pool, domain.DefaultLimits(), 0)
			resp, raw := postBatch(t, base, batchJSON(t, mixedItems(n)), withKey("size-"+name[:1]))
			if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, raw).has("body.notifications") {
				t.Fatalf("status %d %s, want 422 naming body.notifications", resp.StatusCode, raw)
			}
			nothingWritten(t, pool)
		})
	}
}

// TestBatchInvalidItem: one invalid item of ten gets 422 naming its index and
// field, and nothing is written: no batch, no notification, no key (R10, K12).
func TestBatchInvalidItem(t *testing.T) {
	t.Run("structure", func(t *testing.T) {
		pool := testdb.New(t)
		base := batchServer(t, pool, domain.DefaultLimits(), 0)
		items := mixedItems(10)
		items[5].Channel = "fax"
		resp, raw := postBatch(t, base, batchJSON(t, items), withKey("invalid-structure"))
		if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, raw).has("body.notifications[5].channel") {
			t.Fatalf("status %d %s, want 422 naming body.notifications[5].channel", resp.StatusCode, raw)
		}
		nothingWritten(t, pool)
	})

	t.Run("limit", func(t *testing.T) {
		pool := testdb.New(t)
		base := batchServer(t, pool, domain.DefaultLimits(), 0)
		items := mixedItems(10)
		items[2].Recipient, items[2].Channel = "+905551234567", "sms"
		items[2].Content = strings.Repeat("ş", 161) // the sms limit is 160 code points
		resp, raw := postBatch(t, base, batchJSON(t, items), withKey("invalid-limit"))
		p := decodeProblem(t, raw)
		if resp.StatusCode != http.StatusUnprocessableEntity || !p.has("body.notifications[2].content") || len(p.Errors) != 1 {
			t.Fatalf("status %d %s, want 422 naming only body.notifications[2].content", resp.StatusCode, raw)
		}
		nothingWritten(t, pool)
	})
}

// sizedBody returns a valid batch body of exactly size bytes: n email items
// whose contents fill the space.
func sizedBody(t *testing.T, size, n int) []byte {
	t.Helper()
	items := make([]httpapi.NotificationRequest, n)
	for i := range items {
		items[i] = httpapi.NotificationRequest{Recipient: "user@example.com", Channel: "email"}
	}
	free := size - len(batchJSON(t, items))
	for i := range items {
		share := free / n
		if i == n-1 {
			share += free % n
		}
		items[i].Content = strings.Repeat("x", share)
	}
	body := batchJSON(t, items)
	if len(body) != size {
		t.Fatalf("body is %d bytes, want %d", len(body), size)
	}
	return body
}

// TestBatchBodyLimit: a body over the limit gets 413 and writes nothing, with
// the default 16 MiB and with a configured 1 MiB; a body under 1 MiB is
// accepted (R2, K12).
func TestBatchBodyLimit(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		pool := testdb.New(t)
		// Content limits high enough that the body would be valid if it fit.
		base := batchServer(t, pool, domain.Limits{SMS: 160, Email: 20000, Push: 1000}, 0)
		body := sizedBody(t, httpapi.DefaultBatchMaxBodyBytes+1, domain.MaxBatchItems)
		resp, raw := postBatch(t, base, body, withKey("body-default-over"))
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("%d bytes: status %d %s, want 413", len(body), resp.StatusCode, raw)
		}
		nothingWritten(t, pool)
	})

	t.Run("configured", func(t *testing.T) {
		pool := testdb.New(t)
		const limit = 1 << 20
		base := batchServer(t, pool, domain.DefaultLimits(), limit)
		resp, raw := postBatch(t, base, sizedBody(t, limit+1, 200), withKey("body-configured-over"))
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("1 MiB + 1: status %d %s, want 413", resp.StatusCode, raw)
		}
		nothingWritten(t, pool)

		resp, raw = postBatch(t, base, sizedBody(t, limit-1, 200), withKey("body-configured-under"))
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("1 MiB - 1: status %d %s, want 202", resp.StatusCode, raw)
		}
		if n := rows(t, pool, "notifications"); n != 200 {
			t.Errorf("%d notifications, want 200", n)
		}
	})
}

// TestBatchResponseOrder: notification_ids[i] is the row stored at
// batch_position i with item i's content (K12).
func TestBatchResponseOrder(t *testing.T) {
	pool := testdb.New(t)
	base := batchServer(t, pool, domain.DefaultLimits(), 0)
	items := mixedItems(20)
	resp, raw := postBatch(t, base, batchJSON(t, items), nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	a := decodeBatchAccepted(t, raw)
	if len(a.NotificationIDs) != 20 {
		t.Fatalf("%d IDs, want 20", len(a.NotificationIDs))
	}
	for i, id := range a.NotificationIDs {
		var (
			position int
			content  string
		)
		if err := pool.QueryRow(t.Context(),
			`SELECT batch_position, content FROM notifications WHERE id = $1::text::uuid AND batch_id = $2::text::uuid`,
			id, a.BatchID).Scan(&position, &content); err != nil {
			t.Fatalf("notification_ids[%d] = %s: %v", i, id, err)
		}
		if position != i || content != items[i].Content {
			t.Errorf("notification_ids[%d]: position %d content %q, want %d %q", i, position, content, i, items[i].Content)
		}
	}
}

// TestBatchIdempotentRepeat: the same key and body twice give the same 202
// bytes and write nothing new (R11, K10).
func TestBatchIdempotentRepeat(t *testing.T) {
	pool := testdb.New(t)
	base := batchServer(t, pool, domain.DefaultLimits(), 0)
	body := batchJSON(t, mixedItems(5))
	first, firstRaw := postBatch(t, base, body, withKey("batch-repeat"))
	second, secondRaw := postBatch(t, base, body, withKey("batch-repeat"))
	if first.StatusCode != http.StatusAccepted || second.StatusCode != http.StatusAccepted {
		t.Fatalf("statuses %d, %d; want 202 twice", first.StatusCode, second.StatusCode)
	}
	if !bytes.Equal(firstRaw, secondRaw) {
		t.Errorf("second body %s differs from the first %s", secondRaw, firstRaw)
	}
	if b, n := rows(t, pool, "batches"), rows(t, pool, "notifications"); b != 1 || n != 5 {
		t.Errorf("%d batches and %d notifications, want 1 and 5", b, n)
	}
}

// TestKeyReusedAcrossOperations: a key used for a single create gets 422 when
// sent with a batch, and the batch writes nothing (R11, K10).
func TestKeyReusedAcrossOperations(t *testing.T) {
	pool := testdb.New(t)
	base := batchServer(t, pool, domain.DefaultLimits(), 0)
	if resp, raw := post(t, base, smsBody, withKey("shared-key")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("single create: %d %s", resp.StatusCode, raw)
	}
	resp, raw := postBatch(t, base, batchJSON(t, mixedItems(3)), withKey("shared-key"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, raw).has("header.Idempotency-Key") {
		t.Fatalf("batch with the same key: %d %s, want 422 naming the header", resp.StatusCode, raw)
	}
	if b, n := rows(t, pool, "batches"), rows(t, pool, "notifications"); b != 0 || n != 1 {
		t.Errorf("%d batches and %d notifications, want 0 and 1", b, n)
	}
}

// createBatch posts n mixed items and returns the 202 body.
func createBatch(t *testing.T, base string, n int, header http.Header) httpapi.BatchAcceptedBody {
	t.Helper()
	resp, raw := postBatch(t, base, batchJSON(t, mixedItems(n)), header)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("create batch: %d %s", resp.StatusCode, raw)
	}
	return decodeBatchAccepted(t, raw)
}

// setStatuses gives the batch's items the statuses in order of position.
func setStatuses(t *testing.T, pool *pgxpool.Pool, batchID string, statuses []domain.Status) {
	t.Helper()
	for i, st := range statuses {
		if _, err := pool.Exec(t.Context(),
			`UPDATE notifications
			 SET status = $3::text::notification_status,
			     lease_token = CASE WHEN $3 = 'processing' THEN gen_random_uuid() END,
			     lease_expires_at = CASE WHEN $3 = 'processing' THEN now() + interval '30 seconds' END
			 WHERE batch_id = $1::text::uuid AND batch_position = $2`, batchID, i, string(st)); err != nil {
			t.Fatal(err)
		}
	}
}

func getBatch(t *testing.T, base, id string, query url.Values) (int, httpapi.BatchBody, []byte) {
	t.Helper()
	resp, raw := do(t, http.MethodGet, base+"/batches/"+id+"?"+query.Encode())
	var b httpapi.BatchBody
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode, b, raw
}

func repeat(st domain.Status, n int) []domain.Status {
	return slices.Repeat([]domain.Status{st}, n)
}

// TestBatchStatusCounts: the counts match the statuses exactly, and a status
// no item has is present with 0 (R4, K12).
func TestBatchStatusCounts(t *testing.T) {
	t.Run("all", func(t *testing.T) {
		pool := testdb.New(t)
		base := batchServer(t, pool, domain.DefaultLimits(), 0)
		a := createBatch(t, base, 12, nil)
		setStatuses(t, pool, a.BatchID, slices.Concat(repeat(domain.StatusQueued, 3), repeat(domain.StatusProcessing, 2),
			repeat(domain.StatusSent, 4), repeat(domain.StatusFailed, 2), repeat(domain.StatusCancelled, 1)))
		code, b, raw := getBatch(t, base, a.BatchID, nil)
		want := httpapi.BatchCounts{Queued: 3, Processing: 2, Sent: 4, Failed: 2, Cancelled: 1}
		if code != http.StatusOK || b.Counts != want || b.ItemCount != 12 || b.ID != a.BatchID || len(b.Items) != 12 {
			t.Fatalf("status %d %s, want counts %+v, item_count 12 and 12 items", code, raw, want)
		}
	})

	t.Run("missing", func(t *testing.T) {
		pool := testdb.New(t)
		base := batchServer(t, pool, domain.DefaultLimits(), 0)
		a := createBatch(t, base, 5, nil)
		setStatuses(t, pool, a.BatchID, slices.Concat(repeat(domain.StatusQueued, 2), repeat(domain.StatusSent, 3)))
		code, _, raw := getBatch(t, base, a.BatchID, nil)
		var body struct {
			Counts map[string]*int `json:"counts"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || code != http.StatusOK {
			t.Fatalf("status %d %s: %v", code, raw, err)
		}
		want := map[string]int{"queued": 2, "processing": 0, "sent": 3, "failed": 0, "cancelled": 0}
		for st, n := range want {
			if got := body.Counts[st]; got == nil || *got != n {
				t.Errorf("counts.%s = %v, want present with %d", st, got, n)
			}
		}
	})
}

// TestBatchStatusPages: walking batch A with limit=7 returns each of its 25
// IDs exactly once, every item carries A's ID, and batch B, created while A
// is being read, never appears (R4, K12, K13).
func TestBatchStatusPages(t *testing.T) {
	pool := testdb.New(t)
	base := batchServer(t, pool, domain.DefaultLimits(), 0)
	a := createBatch(t, base, 25, nil)
	var (
		seen   []string
		b      httpapi.BatchAcceptedBody
		cursor string
		pages  int
	)
	for {
		q := url.Values{"limit": {"7"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		code, page, raw := getBatch(t, base, a.BatchID, q)
		if code != http.StatusOK {
			t.Fatalf("page %d: %d %s", pages, code, raw)
		}
		pages++
		for _, it := range page.Items {
			if it.BatchID == nil || *it.BatchID != a.BatchID {
				t.Fatalf("item %s has batch_id %v, want %s", it.ID, it.BatchID, a.BatchID)
			}
			seen = append(seen, it.ID)
		}
		if pages == 1 {
			b = createBatch(t, base, 10, nil)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := slices.Clone(a.NotificationIDs)
	slices.Sort(want)
	got := slices.Clone(seen)
	slices.Sort(got)
	if pages != 4 || !slices.Equal(got, want) {
		t.Fatalf("%d pages with %d IDs, want 4 pages with A's 25 IDs once each", pages, len(seen))
	}
	for _, id := range b.NotificationIDs {
		if slices.Contains(seen, id) {
			t.Errorf("batch B's %s appeared in batch A's pages", id)
		}
	}
}

// TestGetBatchErrors: unknown → 404, malformed → 422, a cursor no page
// produced → 400.
func TestGetBatchErrors(t *testing.T) {
	pool := testdb.New(t)
	base := batchServer(t, pool, domain.DefaultLimits(), 0)
	a := createBatch(t, base, 2, nil)
	for name, tc := range map[string]struct {
		id    string
		query url.Values
		want  int
	}{
		"unknown":   {"01a1f9e9-6831-7926-8408-d50f412669df", nil, http.StatusNotFound},
		"malformed": {"not-a-uuid", nil, http.StatusUnprocessableEntity},
		"cursor":    {a.BatchID, url.Values{"cursor": {"broken"}}, http.StatusBadRequest},
		"limit":     {a.BatchID, url.Values{"limit": {"201"}}, http.StatusUnprocessableEntity},
	} {
		if code, _, raw := getBatch(t, base, tc.id, tc.query); code != tc.want {
			t.Errorf("%s: %d %s, want %d", name, code, raw, tc.want)
		}
	}
}

// TestBatchCorrelationID: every item and the batch store the request's
// correlation ID; the notification IDs differ (R15, K21).
func TestBatchCorrelationID(t *testing.T) {
	pool := testdb.New(t)
	base := batchServer(t, pool, domain.DefaultLimits(), 0)
	a := createBatch(t, base, 6, http.Header{httpapi.CorrelationIDHeader: {"batch-1"}})
	var items, withID, distinct int
	var batchCorr string
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*), count(*) FILTER (WHERE correlation_id = 'batch-1'), count(DISTINCT id),
		        (SELECT correlation_id FROM batches WHERE id = $1::text::uuid)
		 FROM notifications WHERE batch_id = $1::text::uuid`, a.BatchID).Scan(&items, &withID, &distinct, &batchCorr); err != nil {
		t.Fatal(err)
	}
	if items != 6 || withID != 6 || distinct != 6 || batchCorr != "batch-1" {
		t.Errorf("items %d, with batch-1 %d, distinct IDs %d, batch correlation %q; want 6, 6, 6, batch-1",
			items, withID, distinct, batchCorr)
	}
}
