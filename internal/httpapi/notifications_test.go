package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/testdb"
)

const smsBody = `{"recipient":"+905551234567","channel":"sms","content":"hello"}`

func notificationServer(t *testing.T, pool *pgxpool.Pool, limits domain.Limits) string {
	t.Helper()
	srv := httptest.NewServer(httpapi.New(httpapi.Deps{
		Logger:        slog.New(slog.DiscardHandler),
		Readiness:     &httpapi.Readiness{},
		Notifications: service.NewNotifications(postgres.NewStore(pool), limits),
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// post sends body to /notifications with the given header lines.
func post(t *testing.T, baseURL, body string, header http.Header) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/notifications", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, values := range header {
		req.Header[name] = values
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

func withKey(key string) http.Header {
	return http.Header{httpapi.IdempotencyKeyHeader: {key}}
}

func rows(t *testing.T, pool *pgxpool.Pool, table string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func decodeAccepted(t *testing.T, body []byte) httpapi.AcceptedBody {
	t.Helper()
	var a httpapi.AcceptedBody
	if err := json.Unmarshal(body, &a); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return a
}

func TestCreateNotification(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	resp, body := post(t, base, smsBody, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d, want 202; body %s", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	accepted := decodeAccepted(t, body)
	if accepted.Status != "queued" || accepted.ID == "" {
		t.Fatalf("body = %s, want an id and status queued", body)
	}
	var version int
	var priority string
	if err := pool.QueryRow(t.Context(),
		`SELECT uuid_extract_version(id), priority::text FROM notifications WHERE id = $1::uuid`, accepted.ID).Scan(&version, &priority); err != nil {
		t.Fatalf("saved row: %v", err)
	}
	if version != 7 || priority != "normal" {
		t.Errorf("uuid version %d, priority %s; want 7 and normal", version, priority)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
}

func TestGetNotification(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	_, body := post(t, base, `{"recipient":"user@example.com","channel":"email","content":"Merhaba","priority":"high"}`, nil)
	id := decodeAccepted(t, body).ID

	resp, body := do(t, http.MethodGet, base+"/notifications/"+id)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET: status %d; body %s", resp.StatusCode, body)
	}
	var n httpapi.NotificationBody
	if err := json.Unmarshal(body, &n); err != nil {
		t.Fatal(err)
	}
	if n.ID != id || n.Recipient != "user@example.com" || n.Channel != "email" || n.Content != "Merhaba" ||
		n.Priority != "high" || n.Status != "queued" || n.Attempts != 0 || n.LastError != nil || n.CreatedAt.IsZero() {
		t.Errorf("GET body = %s", body)
	}
	for _, field := range []string{"last_error", "provider_message_id", "provider_timestamp", "sent_at"} {
		if !bytes.Contains(body, []byte(`"`+field+`":null`)) {
			t.Errorf("%s is not null in %s", field, body)
		}
	}

	if resp, _ := do(t, http.MethodGet, base+"/notifications/01900000-0000-7000-8000-000000000000"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown UUID: status %d, want 404", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, base+"/notifications/not-a-uuid"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("not-a-uuid: status %d, want 422", resp.StatusCode)
	}
}

func TestCreateValidationErrors(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	t.Run("malformed JSON", func(t *testing.T) {
		resp, body := post(t, base, `{"recipient":"SECRET-RECIPIENT","channel":`, withKey("v-malformed"))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("status %d, want 400; body %s", resp.StatusCode, body)
		}
		if bytes.Contains(body, []byte("SECRET-RECIPIENT")) {
			t.Errorf("the request body was echoed: %s", body)
		}
		if p := decodeProblem(t, body); len(p.Errors) != 1 {
			t.Errorf("want only the parse error, got %s", body)
		}
	})

	t.Run("structure violations", func(t *testing.T) {
		resp, body := post(t, base, `{"recipient":"05551234567","channel":"fax","content":""}`, withKey("v-structure"))
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("status %d, want 422; body %s", resp.StatusCode, body)
		}
		p := decodeProblem(t, body)
		if !p.has("body.channel") || !p.has("body.content") {
			t.Errorf("want errors for channel and content, got %s", body)
		}
		if bytes.Contains(body, []byte(`"value"`)) {
			t.Errorf("an error echoes a value: %s", body)
		}

		resp, body = post(t, base, `{"recipient":"05551234567","channel":"sms","content":"hi"}`, withKey("v-e164"))
		if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("body.recipient") {
			t.Errorf("bad E.164: status %d body %s, want 422 on body.recipient", resp.StatusCode, body)
		}
	})

	t.Run("NUL character", func(t *testing.T) {
		resp, body := post(t, base, `{"recipient":"+905551234567","channel":"sms","content":"a\u0000b"}`, withKey("v-nul"))
		if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("body.content") {
			t.Errorf("status %d body %s, want 422 on body.content", resp.StatusCode, body)
		}
	})

	t.Run("content over the limit", func(t *testing.T) {
		content := strings.Repeat("a", 161)
		resp, body := post(t, base, `{"recipient":"+905551234567","channel":"sms","content":"`+content+`"}`, withKey("v-limit"))
		if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("body.content") {
			t.Errorf("status %d body %s, want 422 on body.content", resp.StatusCode, body)
		}
	})

	if n := rows(t, pool, "notifications"); n != 0 {
		t.Errorf("%d notifications saved, want 0", n)
	}
	if n := rows(t, pool, "idempotency_keys"); n != 0 {
		t.Errorf("%d keys stored, want 0", n)
	}
}

func TestIdempotencyKeyHeader(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	for name, header := range map[string]http.Header{
		"empty":      {httpapi.IdempotencyKeyHeader: {""}},
		"129 bytes":  withKey(strings.Repeat("k", 129)),
		"space":      withKey("has space"),
		"slash":      withKey("a/b"),
		"sent twice": {httpapi.IdempotencyKeyHeader: {"key-one", "key-two"}},
	} {
		resp, body := post(t, base, smsBody, header)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body %s", name, resp.StatusCode, body)
			continue
		}
		if !decodeProblem(t, body).has("header.Idempotency-Key") {
			t.Errorf("%s: the error does not name the header: %s", name, body)
		}
	}
	if n, k := rows(t, pool, "notifications"), rows(t, pool, "idempotency_keys"); n != 0 || k != 0 {
		t.Fatalf("after bad keys: %d notifications, %d keys; want 0 and 0", n, k)
	}

	resp, body := post(t, base, smsBody, withKey(strings.Repeat("k", 128)))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("128-byte key: status %d, want 202; body %s", resp.StatusCode, body)
	}
}

func TestIdempotentRepeat(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	first, firstBody := post(t, base, smsBody, withKey("repeat-1"))
	second, secondBody := post(t, base, smsBody, withKey("repeat-1"))
	if first.StatusCode != http.StatusAccepted || second.StatusCode != http.StatusAccepted {
		t.Fatalf("statuses %d and %d, want 202 twice", first.StatusCode, second.StatusCode)
	}
	if !bytes.Equal(firstBody, secondBody) {
		t.Fatalf("replay differs:\nfirst  %s\nsecond %s", firstBody, secondBody)
	}
	if ct := second.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("replay Content-Type = %q, want application/json", ct)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
}

func TestReplayAfterLimitLowered(t *testing.T) {
	pool := testdb.New(t)
	original := domain.DefaultLimits() // sms 160
	lowered := original
	lowered.SMS = 100
	before := notificationServer(t, pool, original)
	after := notificationServer(t, pool, lowered)

	body := `{"recipient":"+905551234567","channel":"sms","content":"` + strings.Repeat("ç", 120) + `"}`
	resp, firstBody := post(t, before, body, withKey("k1"))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first request: status %d; body %s", resp.StatusCode, firstBody)
	}

	resp, replayBody := post(t, after, body, withKey("k1"))
	if resp.StatusCode != http.StatusAccepted || !bytes.Equal(firstBody, replayBody) {
		t.Fatalf("replay after lowering: status %d body %s, want the first 202 %s", resp.StatusCode, replayBody, firstBody)
	}

	if resp, b := post(t, after, body, withKey("k2")); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("new key k2: status %d, want 422; body %s", resp.StatusCode, b)
	}
	if resp, b := post(t, after, body, nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("no key: status %d, want 422; body %s", resp.StatusCode, b)
	}

	if n := rows(t, pool, "notifications"); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
	var k2 int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM idempotency_keys WHERE key = 'k2'`).Scan(&k2); err != nil || k2 != 0 {
		t.Errorf("key k2 stored (%d, %v), want none", k2, err)
	}
}

func TestIdempotencyKeyConflict(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	if resp, b := post(t, base, smsBody, withKey("conflict-1")); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first: status %d; body %s", resp.StatusCode, b)
	}
	resp, body := post(t, base, `{"recipient":"+905551234567","channel":"sms","content":"different"}`, withKey("conflict-1"))
	if resp.StatusCode != http.StatusUnprocessableEntity || !decodeProblem(t, body).has("header.Idempotency-Key") {
		t.Fatalf("different body: status %d body %s, want 422 naming the header", resp.StatusCode, body)
	}
	if n := rows(t, pool, "notifications"); n != 1 {
		t.Errorf("%d rows, want 1", n)
	}
}

func TestCreateWithoutKey(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	_, first := post(t, base, smsBody, nil)
	_, second := post(t, base, smsBody, nil)
	if decodeAccepted(t, first).ID == decodeAccepted(t, second).ID {
		t.Error("two requests without a key returned the same ID")
	}
	if n := rows(t, pool, "notifications"); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

var generatedID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func TestCorrelationIDHeader(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())

	stored := func(t *testing.T, body []byte) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(t.Context(), `SELECT correlation_id FROM notifications WHERE id = $1::uuid`,
			decodeAccepted(t, body).ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("valid header is used", func(t *testing.T) {
		resp, body := post(t, base, smsBody, http.Header{httpapi.CorrelationIDHeader: {"test-123"}})
		if got := resp.Header.Get(httpapi.CorrelationIDHeader); got != "test-123" {
			t.Errorf("response header = %q, want test-123", got)
		}
		if got := stored(t, body); got != "test-123" {
			t.Errorf("stored = %q, want test-123", got)
		}
	})

	for name, header := range map[string]http.Header{
		"invalid header": {httpapi.CorrelationIDHeader: {"not valid!"}},
		"too long":       {httpapi.CorrelationIDHeader: {strings.Repeat("a", 65)}},
		"no header":      nil,
	} {
		t.Run(name+" is replaced", func(t *testing.T) {
			resp, body := post(t, base, smsBody, header)
			got := resp.Header.Get(httpapi.CorrelationIDHeader)
			if !generatedID.MatchString(got) || (header != nil && got == header.Get(httpapi.CorrelationIDHeader)) {
				t.Fatalf("response header = %q, want a generated ID", got)
			}
			if s := stored(t, body); s != got {
				t.Errorf("stored = %q, want the generated %q", s, got)
			}
		})
	}
}

func decodeProblem(t *testing.T, body []byte) problem {
	t.Helper()
	var p problem
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode problem %s: %v", body, err)
	}
	return p
}

func (p problem) has(location string) bool {
	for _, e := range p.Errors {
		if e.Location == location {
			return true
		}
	}
	return false
}

// cancel posts to /notifications/{id}/cancel.
func cancel(t *testing.T, baseURL, id string) (reply, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, baseURL+"/notifications/"+id+"/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST cancel: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply{StatusCode: resp.StatusCode, Header: resp.Header}, b
}

// TestCancelResponses: queued → 200 cancelled; cancelled → 200 again;
// processing, sent and failed → 409 with the current status; unknown → 404;
// malformed → 422 (R5, K9).
func TestCancelResponses(t *testing.T) {
	pool := testdb.New(t)
	base := notificationServer(t, pool, domain.DefaultLimits())
	create := func(status string) string {
		t.Helper()
		_, body := post(t, base, smsBody, nil)
		id := decodeAccepted(t, body).ID
		if status == "queued" {
			return id
		}
		lease := "NULL"
		if status == "processing" {
			lease = "gen_random_uuid()"
		}
		if _, err := pool.Exec(t.Context(),
			`UPDATE notifications SET status = $2::notification_status, lease_token = `+lease+`,
			        lease_expires_at = CASE WHEN $2 = 'processing' THEN now() + interval '30 seconds' END
			 WHERE id = $1::uuid`, id, status); err != nil {
			t.Fatal(err)
		}
		return id
	}

	queued := create("queued")
	resp, body := cancel(t, base, queued)
	var n httpapi.NotificationBody
	if err := json.Unmarshal(body, &n); err != nil || resp.StatusCode != http.StatusOK || n.Status != "cancelled" || n.ID != queued {
		t.Fatalf("cancel queued: status %d, body %s; want 200 with status cancelled", resp.StatusCode, body)
	}
	resp, body = cancel(t, base, queued)
	if err := json.Unmarshal(body, &n); err != nil || resp.StatusCode != http.StatusOK || n.Status != "cancelled" {
		t.Fatalf("cancel again: status %d, body %s; want 200 with status cancelled", resp.StatusCode, body)
	}

	for _, status := range []string{"processing", "sent", "failed"} {
		id := create(status)
		resp, body := cancel(t, base, id)
		var c httpapi.CancelConflictBody
		if err := json.Unmarshal(body, &c); err != nil || resp.StatusCode != http.StatusConflict ||
			c.CurrentStatus != status || c.Status != http.StatusConflict ||
			resp.Header.Get("Content-Type") != "application/problem+json" {
			t.Errorf("cancel %s: status %d, Content-Type %q, body %s; want 409 problem with current_status %s",
				status, resp.StatusCode, resp.Header.Get("Content-Type"), body, status)
		}
		var still string
		if err := pool.QueryRow(t.Context(), `SELECT status::text FROM notifications WHERE id = $1::uuid`, id).Scan(&still); err != nil || still != status {
			t.Errorf("after a refused cancel the row is %s, want %s unchanged", still, status)
		}
	}

	if resp, body := cancel(t, base, "0190f5a0-0000-7000-8000-000000000000"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ID: status %d, body %s; want 404", resp.StatusCode, body)
	}
	if resp, body := cancel(t, base, "not-a-uuid"); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("malformed ID: status %d, body %s; want 422", resp.StatusCode, body)
	}
}
