package provider_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
)

// TestClassify checks every row of the K7 classification under both profiles,
// and that each code K7 does not list is terminal with its class and code in
// last_error (K41).
func TestClassify(t *testing.T) {
	timeout := &provider.TransportError{Err: context.DeadlineExceeded}
	refused := &provider.TransportError{Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused")}

	type row struct {
		name   string
		code   int
		err    error
		class  provider.Class
		errMsg string // empty: not checked beyond the class prefix
	}
	same := func(code int, class provider.Class) row {
		return row{name: fmt.Sprint(code), code: code, class: class}
	}
	common := []row{
		{name: "202", code: 202, class: provider.ClassAccepted},
		{name: "timeout", err: timeout, class: provider.ClassTemporary, errMsg: "temporary: provider call: context deadline exceeded"},
		{name: "network error", err: refused, class: provider.ClassTemporary},
		{name: "request not built", err: errors.New("encode request: x"), class: provider.ClassPermanent},
		same(408, provider.ClassTemporary),
		same(500, provider.ClassTemporary),
		same(502, provider.ClassTemporary),
		{name: "503", code: 503, class: provider.ClassTemporary, errMsg: "temporary: status 503"},
		same(504, provider.ClassTemporary),
		{name: "400", code: 400, class: provider.ClassPermanent, errMsg: "permanent: status 400"},
		same(501, provider.ClassPermanent),
	}
	// K41: every other 2xx, 3xx, 4xx and 5xx.
	for _, code := range []int{200, 204, 301, 307, 404, 409, 505} {
		common = append(common, row{name: fmt.Sprint(code), code: code, class: provider.ClassTerminal,
			errMsg: fmt.Sprintf("terminal: unexpected status %d", code)})
	}

	for _, profile := range []provider.Profile{provider.ProfileStandard, provider.ProfileWebhookSiteFree} {
		rows := append([]row(nil), common...)
		if profile == provider.ProfileStandard {
			rows = append(rows, row{name: "429", code: 429, class: provider.ClassTemporary, errMsg: "temporary: status 429"})
		} else {
			rows = append(rows, row{name: "429", code: 429, class: provider.ClassPermanent,
				errMsg: "permanent: status 429 (webhook.site quota used up)"})
		}
		for _, r := range rows {
			t.Run(string(profile)+"/"+r.name, func(t *testing.T) {
				got := provider.Classify(provider.Response{StatusCode: r.code}, r.err, profile)
				if got.Class != r.class {
					t.Fatalf("class %s, want %s", got.Class, r.class)
				}
				switch {
				case r.class == provider.ClassAccepted && got.Error != "":
					t.Errorf("accepted with error %q", got.Error)
				case r.class != provider.ClassAccepted && r.errMsg == "" && !strings.HasPrefix(got.Error, string(r.class)+": "):
					t.Errorf("error %q does not start with the class", got.Error)
				case r.errMsg != "" && got.Error != r.errMsg:
					t.Errorf("error %q, want %q", got.Error, r.errMsg)
				}
			})
		}
	}
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// TestParseRetryAfter reads seconds and HTTP dates and ignores garbage (K7).
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name  string
		value string
		want  time.Duration
		ok    bool
	}{
		{"seconds", "2", 2 * time.Second, true},
		{"zero seconds", "0", 0, true},
		{"seconds with spaces", " 120 ", 2 * time.Minute, true},
		{"HTTP date", "Fri, 20 Nov 2026 12:01:30 GMT", 90 * time.Second, true},
		{"past HTTP date", "Fri, 20 Nov 2026 11:00:00 GMT", 0, true},
		{"empty", "", 0, false},
		{"negative", "-5", 0, false},
		{"fraction", "1.5", 0, false},
		{"garbage", "soon", 0, false},
		{"huge number", "99999999999999999999", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := provider.ParseRetryAfter(tt.value, now)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ParseRetryAfter(%q) = %s, %v; want %s, %v", tt.value, got, ok, tt.want, tt.ok)
			}
		})
	}
}

// TestRetryAfterHeaderReturned: Send hands a non-202 response's Retry-After
// back unchanged.
func TestRetryAfterHeaderReturned(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	resp, err := provider.NewClient(srv.URL, time.Second, 1).Send(t.Context(), provider.Request{To: "x", Channel: "sms", Content: "c"})
	if err != nil || resp.StatusCode != 503 || resp.RetryAfter != "7" {
		t.Fatalf("resp %+v, err %v; want 503 with Retry-After 7", resp, err)
	}
}
