package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrorCode classifies a field error.
type ErrorCode string

// Field error codes.
const (
	// CodeRequired: the field is missing or empty.
	CodeRequired ErrorCode = "required"
	// CodeUnknownValue: the value is not one of the allowed values.
	CodeUnknownValue ErrorCode = "unknown_value"
	// CodeFormat: the recipient does not match its channel's format.
	CodeFormat ErrorCode = "format"
	// CodeNUL: the text contains U+0000, which PostgreSQL text cannot hold.
	CodeNUL ErrorCode = "nul_character"
	// CodeTooLong: the content is over its channel's configured limit.
	CodeTooLong ErrorCode = "too_long"
	// CodePast: scheduled_at is at or before the database clock (K26).
	CodePast ErrorCode = "in_the_past"
)

// FieldError is one rule violation.
type FieldError struct {
	Field   string
	Code    ErrorCode
	Message string
}

// ValidationError lists every rule violation of a request.
type ValidationError struct {
	Errors []FieldError
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Errors))
	for i, fe := range e.Errors {
		parts[i] = fe.Field + ": " + fe.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

const maxPushTokenLength = 512

// e164 is "+" followed by 2 to 15 digits, the first not 0.
var e164 = regexp.MustCompile(`^\+[1-9][0-9]{1,14}$`)

// Normalize checks the structure rules of K11 and applies the defaults
// (priority → normal). Single and batch creation share it. The configurable
// content limits are separate (Limits.Check), because only a request that is
// about to save a new notification checks them (K10).
func Normalize(n NewNotification) (NewNotification, []FieldError) {
	var errs []FieldError
	add := func(field string, code ErrorCode, msg string) {
		errs = append(errs, FieldError{Field: field, Code: code, Message: msg})
	}

	channelKnown := false
	switch n.Channel {
	case "":
		add("channel", CodeRequired, "is required")
	case ChannelSMS, ChannelEmail, ChannelPush:
		channelKnown = true
	default:
		add("channel", CodeUnknownValue, "must be sms, email or push")
	}

	switch n.Priority {
	case "":
		n.Priority = PriorityNormal
	case PriorityHigh, PriorityNormal, PriorityLow:
	default:
		add("priority", CodeUnknownValue, "must be high, normal or low")
	}

	switch {
	case n.Recipient == "":
		add("recipient", CodeRequired, "is required")
	case strings.ContainsRune(n.Recipient, 0):
		add("recipient", CodeNUL, "must not contain the NUL character (U+0000)")
	case channelKnown:
		if msg := recipientFormat(n.Channel, n.Recipient); msg != "" {
			add("recipient", CodeFormat, msg)
		}
	}

	switch {
	case n.Content == "":
		add("content", CodeRequired, "is required")
	case strings.ContainsRune(n.Content, 0):
		add("content", CodeNUL, "must not contain the NUL character (U+0000)")
	}

	if n.ScheduledAt != "" {
		at, err := ParseScheduledAt(n.ScheduledAt)
		if err != nil {
			add("scheduled_at", CodeFormat, ScheduledAtFormat)
		} else {
			n.ScheduledAt = at.Format(time.RFC3339Nano)
		}
	}

	return n, errs
}

// ScheduledAtFormat is the message for a scheduled_at that does not parse.
const ScheduledAtFormat = "must be an RFC 3339 time with an offset (Z or ±hh:mm), for example 2026-11-22T09:00:00Z"

// ParseScheduledAt reads an RFC 3339 time that carries its offset, Z or
// ±hh:mm, and returns it in UTC (K26). A time without an offset is refused:
// it would mean a different moment on every server.
func ParseScheduledAt(s string) (time.Time, error) {
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, err
	}
	return at.UTC(), nil
}

// PastError returns the field error for a scheduled_at at or before the
// database clock; field names the item in a batch.
func PastError(field string) FieldError {
	return FieldError{Field: field, Code: CodePast, Message: "must be after the current time"}
}

// NormalizeBatch runs Normalize on every item of a batch and collects every
// error. Each error's field names its item, as in "notifications[7].recipient",
// so a client can find the item it has to fix (K11, K12).
func NormalizeBatch(items []NewNotification) ([]NewNotification, []FieldError) {
	out := make([]NewNotification, len(items))
	var errs []FieldError
	for i, item := range items {
		n, itemErrs := Normalize(item)
		out[i] = n
		errs = append(errs, ItemErrors(i, itemErrs)...)
	}
	return out, errs
}

// ItemErrors prefixes each field with the batch item it belongs to.
func ItemErrors(index int, errs []FieldError) []FieldError {
	for i := range errs {
		errs[i].Field = fmt.Sprintf("notifications[%d].%s", index, errs[i].Field)
	}
	return errs
}

// recipientFormat returns why recipient is invalid for channel, or "".
func recipientFormat(channel Channel, recipient string) string {
	switch channel {
	case ChannelSMS:
		if !e164.MatchString(recipient) {
			return "must be an E.164 phone number: + and up to 15 digits, for example +905551234567"
		}
	case ChannelEmail:
		addr, err := mail.ParseAddress(recipient)
		if err != nil || addr.Address != recipient {
			return "must be a plain email address such as user@example.com, without a display name"
		}
	case ChannelPush:
		if strings.IndexFunc(recipient, unicode.IsSpace) >= 0 {
			return "must not contain whitespace"
		}
		if utf8.RuneCountInString(recipient) > maxPushTokenLength {
			return fmt.Sprintf("must be at most %d characters", maxPushTokenLength)
		}
	}
	return ""
}

// Limits are the configurable content limits per channel, in Unicode code
// points (K11). They are application limits, not carrier guarantees.
type Limits struct {
	SMS   int
	Email int
	Push  int
}

// DefaultLimits returns the documented defaults.
func DefaultLimits() Limits {
	return Limits{SMS: 160, Email: 10000, Push: 1000}
}

// Check returns a field error when the content is over its channel's limit.
// It expects a notification that passed Normalize.
func (l Limits) Check(n NewNotification) []FieldError {
	limit := 0
	switch n.Channel {
	case ChannelSMS:
		limit = l.SMS
	case ChannelEmail:
		limit = l.Email
	case ChannelPush:
		limit = l.Push
	}
	if count := utf8.RuneCountInString(n.Content); limit > 0 && count > limit {
		return []FieldError{{
			Field:   "content",
			Code:    CodeTooLong,
			Message: fmt.Sprintf("must be at most %d characters for %s, got %d", limit, n.Channel, count),
		}}
	}
	return nil
}

// hashedRequest fixes the field order of the hashed encoding.
type hashedRequest struct {
	Operation Operation    `json:"operation"`
	Items     []hashedItem `json:"items"`
}

type hashedItem struct {
	Recipient string   `json:"recipient"`
	Channel   Channel  `json:"channel"`
	Content   string   `json:"content"`
	Priority  Priority `json:"priority"`
	// Left out when empty, so a request without scheduled_at hashes as it
	// did before the field existed and its stored key still replays.
	ScheduledAt string `json:"scheduled_at,omitempty"`
}

// RequestHash returns the SHA-256 of the operation and its normalized items
// (K10). Callers pass items after Normalize, so a missing priority and an
// explicit "normal" hash the same.
func RequestHash(op Operation, items []NewNotification) []byte {
	req := hashedRequest{Operation: op, Items: make([]hashedItem, len(items))}
	for i, n := range items {
		// A conversion, not a literal: a new NewNotification field stops the
		// build here until the hash decides whether to include it.
		req.Items[i] = hashedItem(n)
	}
	// Marshalling strings and fixed struct fields cannot fail.
	b, _ := json.Marshal(req)
	sum := sha256.Sum256(b)
	return sum[:]
}
