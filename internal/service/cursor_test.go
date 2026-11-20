package service_test

import (
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
)

// TestCursorRoundTrip: a cursor decodes to the values it was made from, at
// microsecond precision; anything else is invalid (K13).
func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []domain.Cursor{
		{CreatedAt: time.Date(2026, 11, 20, 10, 30, 0, 123456000, time.UTC), ID: "019a8f3e-1c2d-7e4f-8a9b-0c1d2e3f4a5b"},
		{CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 1000, time.FixedZone("TRT", 3*3600)), ID: "019a8f3e-1c2d-7e4f-8a9b-0c1d2e3f4a5c"},
	} {
		got, err := service.DecodeCursor(service.EncodeCursor(c))
		if err != nil || !got.CreatedAt.Equal(c.CreatedAt) || got.ID != c.ID {
			t.Errorf("round trip of %+v: %+v err=%v", c, got, err)
		}
	}
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	for name, raw := range map[string]string{
		"not base64":       "not-a-cursor!",
		"not JSON":         b64("hello"),
		"no time":          b64(`{"i":"019a8f3e-1c2d-7e4f-8a9b-0c1d2e3f4a5b"}`),
		"bad time":         b64(`{"c":"yesterday","i":"019a8f3e-1c2d-7e4f-8a9b-0c1d2e3f4a5b"}`),
		"ID not a UUID":    b64(`{"c":"2026-11-20T10:30:00Z","i":"1; DROP TABLE notifications"}`),
		"empty JSON":       b64(`{}`),
		"standard base64+": base64.StdEncoding.EncodeToString([]byte(`{"c":"2026-11-20T10:30:00Z","i":"x"}`)),
	} {
		if _, err := service.DecodeCursor(raw); !errors.Is(err, domain.ErrInvalidCursor) {
			t.Errorf("%s: err %v, want ErrInvalidCursor", name, err)
		}
	}
}
