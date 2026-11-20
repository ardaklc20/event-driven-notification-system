package service

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"time"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// cursorContent is what a cursor carries: the last returned row's created_at
// at full precision and its id (K13). It is base64url-encoded, so a client
// treats it as opaque.
type cursorContent struct {
	CreatedAt time.Time `json:"c"`
	ID        string    `json:"i"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// EncodeCursor returns the opaque form of c.
func EncodeCursor(c domain.Cursor) string {
	b, _ := json.Marshal(cursorContent{CreatedAt: c.CreatedAt.UTC(), ID: c.ID}) // a struct of a time and a string always marshals
	return base64.RawURLEncoding.EncodeToString(b)
}

// DecodeCursor reads a cursor EncodeCursor made. Anything else, including a
// cursor that decodes to a missing time or an ID that is not a UUID, gives
// domain.ErrInvalidCursor.
func DecodeCursor(s string) (domain.Cursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return domain.Cursor{}, domain.ErrInvalidCursor
	}
	var c cursorContent
	if err := json.Unmarshal(b, &c); err != nil || c.CreatedAt.IsZero() || !uuidPattern.MatchString(c.ID) {
		return domain.Cursor{}, domain.ErrInvalidCursor
	}
	return domain.Cursor{CreatedAt: c.CreatedAt, ID: c.ID}, nil
}
