package mockprovider_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/mockprovider"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func send(t *testing.T, base, method, path, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// TestMockProvider: three valid POSTs get 202 in the case's shape with three
// different messageIds; a malformed one gets 400; /stats counts the arrivals
// per channel (K14).
func TestMockProvider(t *testing.T) {
	srv := httptest.NewServer(mockprovider.New(0, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(srv.Close)

	ids := map[string]bool{}
	for _, body := range []string{
		`{"to":"+905551234567","channel":"sms","content":"one"}`,
		`{"to":"+905551234567","channel":"sms","content":"two"}`,
		`{"to":"user@example.com","channel":"email","content":"three"}`,
	} {
		status, resp := send(t, srv.URL, http.MethodPost, "/", body)
		if status != http.StatusAccepted {
			t.Fatalf("status %d, want 202; body %s", status, resp)
		}
		var fields map[string]any
		if err := json.Unmarshal(resp, &fields); err != nil || len(fields) != 3 {
			t.Fatalf("body %s, want exactly {messageId, status, timestamp}", resp)
		}
		var acc mockprovider.Acceptance
		if err := json.Unmarshal(resp, &acc); err != nil {
			t.Fatal(err)
		}
		if !uuidPattern.MatchString(acc.MessageID) || acc.Status != "accepted" {
			t.Errorf("body %s, want a UUID messageId and status accepted", resp)
		}
		if _, err := time.Parse(time.RFC3339Nano, acc.Timestamp); err != nil {
			t.Errorf("timestamp %q is not RFC 3339: %v", acc.Timestamp, err)
		}
		ids[acc.MessageID] = true
	}
	if len(ids) != 3 {
		t.Errorf("%d distinct messageIds, want 3", len(ids))
	}

	for name, body := range map[string]string{
		"not JSON":        `{"to":`,
		"unknown channel": `{"to":"x","channel":"fax","content":"y"}`,
		"missing content": `{"to":"x","channel":"sms"}`,
		"extra field":     `{"to":"x","channel":"sms","content":"y","priority":"high"}`,
	} {
		if status, resp := send(t, srv.URL, http.MethodPost, "/", body); status != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body %s", name, status, resp)
		}
	}

	status, resp := send(t, srv.URL, http.MethodGet, "/stats", "")
	if status != http.StatusOK {
		t.Fatalf("/stats: status %d", status)
	}
	var stats mockprovider.Stats
	if err := json.Unmarshal(resp, &stats); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"sms": 2, "email": 1, "push": 0}
	for ch, n := range want {
		got := stats.Channels[ch]
		if got.Arrivals != n || got.MaxArrivalsPerSecond != n {
			t.Errorf("%s: %+v, want %d arrivals, all within one second", ch, got, n)
		}
	}
}

// TestMockDelay: with a delay the 202 comes after it.
func TestMockDelay(t *testing.T) {
	const delay = 200 * time.Millisecond
	srv := httptest.NewServer(mockprovider.New(delay, slog.New(slog.DiscardHandler)).Handler())
	t.Cleanup(srv.Close)

	start := time.Now()
	status, _ := send(t, srv.URL, http.MethodPost, "/", `{"to":"t","channel":"push","content":"c"}`)
	if took := time.Since(start); status != http.StatusAccepted || took < delay {
		t.Fatalf("status %d after %s, want 202 after at least %s", status, took, delay)
	}
}
