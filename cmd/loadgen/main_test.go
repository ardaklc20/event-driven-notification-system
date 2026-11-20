package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLoadgen(t *testing.T) {
	var calls atomic.Int32
	answer := func(fail bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || r.URL.Path != "/notifications" ||
				body["recipient"] != recipients[body["channel"]] {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if fail && n == 5 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = io.WriteString(w, `{"id":"n-1","status":"queued"}`)
		}
	}

	for _, tt := range []struct {
		name string
		fail bool
		code int
		out  string
	}{
		{"all accepted", false, 0, "creates=30 accepted=30 failed=0"},
		{"one failure", true, 1, "creates=30 accepted=29 failed=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls.Store(0)
			srv := httptest.NewServer(answer(tt.fail))
			defer srv.Close()
			var out, errOut bytes.Buffer
			code := run([]string{"-url", srv.URL, "-per-channel", "10", "-concurrency", "4"}, &out, &errOut)
			if code != tt.code || !strings.HasPrefix(out.String(), tt.out) {
				t.Fatalf("exit %d, output %q, stderr %q; want %d and %q", code, out.String(), errOut.String(), tt.code, tt.out)
			}
		})
	}
}
