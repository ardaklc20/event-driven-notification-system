package provider_test

import (
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
)

// TestParseAcceptance checks that each broken field of a 202 body stays empty
// while the readable ones are kept (K15).
func TestParseAcceptance(t *testing.T) {
	ts := time.Date(2026, 11, 20, 10, 30, 0, 0, time.UTC)
	tests := []struct {
		name      string
		body      string
		messageID string
		timestamp time.Time
		problem   bool
	}{
		{"valid", `{"messageId":"m-1","status":"accepted","timestamp":"2026-11-20T10:30:00Z"}`, "m-1", ts, false},
		{"not JSON", `accepted`, "", time.Time{}, true},
		{"JSON array", `[1,2]`, "", time.Time{}, true},
		{"JSON null", `null`, "", time.Time{}, true},
		{"empty body", ``, "", time.Time{}, true},
		{"messageId a number", `{"messageId":42,"timestamp":"2026-11-20T10:30:00Z"}`, "", ts, true},
		{"messageId empty", `{"messageId":"","timestamp":"2026-11-20T10:30:00Z"}`, "", ts, true},
		{"messageId contains NUL", `{"messageId":"m-\u0000123","timestamp":"2026-11-20T10:30:00Z"}`, "", ts, true},
		{"timestamp missing", `{"messageId":"m-1","status":"accepted"}`, "m-1", time.Time{}, true},
		{"timestamp not RFC 3339", `{"messageId":"m-1","timestamp":"20/11/2026"}`, "m-1", time.Time{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			acc, problem := provider.ParseAcceptance([]byte(tt.body))
			if acc.MessageID != tt.messageID || !acc.Timestamp.Equal(tt.timestamp) {
				t.Errorf("acceptance = %+v, want messageId %q timestamp %s", acc, tt.messageID, tt.timestamp)
			}
			if (problem != "") != tt.problem {
				t.Errorf("problem = %q, want a problem: %v", problem, tt.problem)
			}
		})
	}
}

func TestProfileValid(t *testing.T) {
	for p, want := range map[provider.Profile]bool{
		provider.ProfileStandard:        true,
		provider.ProfileWebhookSiteFree: true,
		"webhook_site":                  false,
		"":                              false,
	} {
		if got := p.Valid(); got != want {
			t.Errorf("%q.Valid() = %v, want %v", p, got, want)
		}
	}
}
