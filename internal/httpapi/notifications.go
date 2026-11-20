package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
)

// NotificationRequest is the body of POST /notifications.
type NotificationRequest struct {
	Recipient string `json:"recipient" minLength:"1" example:"+905551234567" doc:"sms: an E.164 number (+ and up to 15 digits). email: a plain address without a display name. push: a device token, no whitespace, at most 512 characters."`
	Channel   string `json:"channel" enum:"sms,email,push" example:"sms"`
	Content   string `json:"content,omitempty" minLength:"1" example:"Your code is 123456" doc:"The message, or leave it out and send template_id. At most 160 (sms), 10000 (email) or 1000 (push) Unicode code points by default; the limits are configurable and apply to new notifications only."`
	Priority  string `json:"priority,omitempty" enum:"high,normal,low" default:"normal"`
	// K26: an optional absolute time.
	ScheduledAt string `json:"scheduled_at,omitempty" format:"date-time" example:"2026-11-22T09:00:00Z" doc:"Optional. The earliest time the notification may be sent, RFC 3339 with an offset (Z or ±hh:mm); a time without an offset gets 422. A new request whose time is at or before the server's clock gets 422; there is no upper limit. Without it the notification is sent as soon as possible."`
	// K33: a stored template instead of content.
	TemplateID string            `json:"template_id,omitempty" format:"uuid" doc:"A stored template to render instead of content; send exactly one of content and template_id. Its channel must equal channel. An unknown ID gets 422 on this field."`
	Variables  map[string]string `json:"variables,omitempty" doc:"With template_id only: one string value per placeholder, no more and no fewer. Values are inserted as they are; a {{name}} inside a value is not rendered again. The rendered content follows the same limits as content."`
}

func (r NotificationRequest) toDomain() domain.NewNotification {
	return domain.NewNotification{
		Recipient: r.Recipient,
		Channel:   domain.Channel(r.Channel),
		Content:   r.Content,
		Priority:  domain.Priority(r.Priority),
		// Normalize rewrites it in UTC.
		ScheduledAt: r.ScheduledAt,
		TemplateID:  r.TemplateID,
		Variables:   r.Variables,
	}
}

// Resolve runs the shared structure rules (K11) after Huma's schema
// validation. Missing fields and unknown enum values are left to the schema,
// which already reports them; that also keeps a body that failed to parse
// from collecting extra errors here.
func (r *NotificationRequest) Resolve(_ huma.Context, prefix *huma.PathBuffer) []error {
	_, fieldErrs := domain.Normalize(r.toDomain())
	var errs []error
	for _, fe := range fieldErrs {
		// The schema's date-time and uuid formats already report a
		// scheduled_at or a template_id that does not parse.
		if fe.Code == domain.CodeRequired || fe.Code == domain.CodeUnknownValue ||
			(fe.Field == "scheduled_at" && fe.Code == domain.CodeFormat) ||
			(fe.Field == "template_id" && fe.Code == domain.CodeFormat) {
			continue
		}
		errs = append(errs, &huma.ErrorDetail{Location: prefix.With(fe.Field), Message: fe.Message})
	}
	return errs
}

// IdempotencyKeyHeader is the optional key of a create request (K10).
const IdempotencyKeyHeader = "Idempotency-Key"

const maxIdempotencyKeyBytes = 128

var idempotencyKeyChars = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type createNotificationInput struct {
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional. Sent once, 1 to 128 characters from A–Z, a–z, 0–9, '.', '_' and '-'. A repeat with the same key and the same request returns the first 202 response unchanged; the same key with a different request gets 422."`
	Body           NotificationRequest
}

// Resolve checks the Idempotency-Key header.
func (in *createNotificationInput) Resolve(ctx huma.Context) []error {
	return resolveIdempotencyKey(ctx)
}

// resolveIdempotencyKey checks every raw value of the Idempotency-Key header,
// so a repeated header is seen too. A bad key gets 400 and nothing is written
// (K10).
func resolveIdempotencyKey(ctx huma.Context) []error {
	var values []string
	ctx.EachHeader(func(name, value string) {
		if http.CanonicalHeaderKey(name) == IdempotencyKeyHeader {
			values = append(values, value)
		}
	})
	if len(values) == 0 {
		return nil
	}
	if msg := checkIdempotencyKey(values); msg != "" {
		return []error{&badRequestError{huma.ErrorDetail{Location: "header." + IdempotencyKeyHeader, Message: msg}}}
	}
	return nil
}

func checkIdempotencyKey(values []string) string {
	switch v := values[0]; {
	case len(values) > 1:
		return "must be sent once"
	case v == "":
		return "must not be empty"
	case len(v) > maxIdempotencyKeyBytes:
		return "must be at most 128 bytes"
	case !idempotencyKeyChars.MatchString(v):
		return "may contain only A–Z, a–z, 0–9, '.', '_' and '-'"
	}
	return ""
}

// badRequestError is an error detail that makes Huma answer 400 instead of
// its default 422 for request errors.
type badRequestError struct {
	detail huma.ErrorDetail
}

func (e *badRequestError) Error() string                  { return e.detail.Error() }
func (e *badRequestError) GetStatus() int                 { return http.StatusBadRequest }
func (e *badRequestError) ErrorDetail() *huma.ErrorDetail { return &e.detail }

// AcceptedBody is the 202 response of POST /notifications.
type AcceptedBody struct {
	ID     string `json:"id" format:"uuid" doc:"Notification ID (UUIDv7)"`
	Status string `json:"status" enum:"queued" doc:"Always queued: the notification is saved and waits for a worker"`
}

// rawJSONOutput writes stored response bytes unchanged. Huma writes a []byte
// body without serializing it again, so a replay is byte for byte the first
// response; the status and Content-Type are set explicitly (K10, K19).
type rawJSONOutput struct {
	Status      int
	ContentType string `header:"Content-Type"`
	Body        []byte
}

// NotificationBody is a notification as GET returns it.
type NotificationBody struct {
	ID        string  `json:"id" format:"uuid"`
	Recipient string  `json:"recipient"`
	Channel   string  `json:"channel" enum:"sms,email,push"`
	Content   string  `json:"content"`
	Priority  string  `json:"priority" enum:"high,normal,low"`
	Status    string  `json:"status" enum:"queued,processing,sent,failed,cancelled"`
	Attempts  int     `json:"attempts" minimum:"0" doc:"Attempts started so far; an attempt counts when a worker claims the notification"`
	LastError *string `json:"last_error" nullable:"true" doc:"The last delivery error, or null"`
	BatchID   *string `json:"batch_id" nullable:"true" format:"uuid" doc:"The batch the notification was created in, or null for a single create"`
	// K15: what the provider answered with its 202.
	ProviderMessageID *string    `json:"provider_message_id" nullable:"true" doc:"The provider's messageId from its 202 response; null before sent, or when the response body was not usable. Not unique: a provider with a static response gives every notification the same ID."`
	ProviderTimestamp *time.Time `json:"provider_timestamp" nullable:"true" doc:"The time the provider reported in its 202 response, or null"`
	SentAt            *time.Time `json:"sent_at" nullable:"true" doc:"When the provider's acceptance was recorded; set together with status sent"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	ScheduledAt       *time.Time `json:"scheduled_at" nullable:"true" doc:"The earliest send time the request asked for, in UTC, or null when the notification was not scheduled. Retries never change it"`
	TemplateID        *string    `json:"template_id" nullable:"true" format:"uuid" doc:"The template content was rendered from, or null; content is the rendered text"`
}

type notificationOutput struct {
	Body NotificationBody
}

type getNotificationInput struct {
	ID string `path:"id" format:"uuid" doc:"Notification ID"`
}

type listNotificationsInput struct {
	Status  string    `query:"status" enum:"queued,processing,sent,failed,cancelled" doc:"Only notifications with this status"`
	Channel string    `query:"channel" enum:"sms,email,push" doc:"Only notifications on this channel"`
	From    time.Time `query:"from" doc:"Only notifications created at or after this time (RFC 3339, included)"`
	To      time.Time `query:"to" doc:"Only notifications created before this time (RFC 3339, excluded)"`
	Limit   int       `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Page size; outside 1 to 200 gets 422"`
	Cursor  string    `query:"cursor" doc:"The next_cursor of the previous page, unchanged"`
}

// ListBody is one page of GET /notifications.
type ListBody struct {
	Items      []NotificationBody `json:"items" doc:"Newest first: created_at descending, then ID descending"`
	NextCursor string             `json:"next_cursor,omitempty" doc:"Pass as cursor to read the next page; absent on the last page"`
}

type listOutput struct {
	Body ListBody
}

// CancelConflictBody is the 409 answer of a cancel: an RFC 9457 problem with
// the status the notification has now (K9). The problem's own status member
// is the HTTP status, so the notification's is current_status.
type CancelConflictBody struct {
	huma.ErrorModel
	CurrentStatus string `json:"current_status" enum:"processing,sent,failed" doc:"The notification's status, read under its row lock"`
}

func (e *CancelConflictBody) Error() string { return e.Detail }

// GetStatus returns 409.
func (e *CancelConflictBody) GetStatus() int { return http.StatusConflict }

func toBody(n domain.Notification) NotificationBody {
	return NotificationBody{
		ID:                n.ID,
		Recipient:         n.Recipient,
		Channel:           string(n.Channel),
		Content:           n.Content,
		Priority:          string(n.Priority),
		Status:            string(n.Status),
		Attempts:          n.Attempts,
		LastError:         n.LastError,
		BatchID:           n.BatchID,
		ProviderMessageID: n.ProviderMessageID,
		ProviderTimestamp: n.ProviderTimestamp,
		SentAt:            n.SentAt,
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
		ScheduledAt:       n.ScheduledAt,
		TemplateID:        n.TemplateID,
	}
}

func registerNotifications(api huma.API, deps Deps) {
	registry := api.OpenAPI().Components.Schemas
	acceptedSchema := registry.Schema(reflect.TypeFor[AcceptedBody](), true, "AcceptedBody")

	huma.Register(api, huma.Operation{
		OperationID: "create-notification",
		Method:      http.MethodPost,
		Path:        "/notifications",
		Summary:     "Create a notification",
		Description: "Validates the notification, saves it as queued and answers 202 at once, without waiting for delivery. " +
			"With an Idempotency-Key, a repeat of the same request returns the first response byte for byte.",
		Tags:          []string{"Notifications"},
		DefaultStatus: http.StatusAccepted,
		Responses: map[string]*huma.Response{
			"202": {
				Description: "Accepted and queued",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: acceptedSchema}},
			},
		},
		Errors: []int{http.StatusBadRequest, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *createNotificationInput) (*rawJSONOutput, error) {
		logger := loggerFrom(ctx, deps.Logger)
		res, err := deps.Notifications.Create(ctx, service.CreateInput{
			Notification:  in.Body.toDomain(),
			Key:           in.IdempotencyKey,
			CorrelationID: correlationIDFrom(ctx),
		}, marshalAccepted)
		if err != nil {
			return nil, problemFor(ctx, logger, err)
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "notification accepted",
			slog.String("notification_id", res.NotificationID),
			slog.String("channel", in.Body.Channel),
			slog.Bool("replayed", res.Replayed),
		)
		return &rawJSONOutput{Status: res.Status, ContentType: "application/json", Body: res.Body}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-notification",
		Method:      http.MethodGet,
		Path:        "/notifications/{id}",
		Summary:     "Get a notification",
		Description: "Returns the notification's current state. A malformed ID gets 422, an unknown one 404.",
		Tags:        []string{"Notifications"},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *getNotificationInput) (*notificationOutput, error) {
		n, err := deps.Notifications.Get(ctx, in.ID)
		if err != nil {
			return nil, problemFor(ctx, loggerFrom(ctx, deps.Logger), err)
		}
		return &notificationOutput{Body: toBody(n)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-notifications",
		Method:      http.MethodGet,
		Path:        "/notifications",
		Summary:     "List notifications",
		Description: "Lists notifications newest first. status, channel, from and to filter with AND; the created_at range is " +
			"half-open, from included and to excluded. Follow next_cursor to read further pages; notifications created " +
			"meanwhile do not shift them. Each page is a separate query, so a notification whose status changes between " +
			"pages can move into or out of a filtered list. A cursor that was not produced by a previous page gets 400.",
		Tags:   []string{"Notifications"},
		Errors: []int{http.StatusBadRequest, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *listNotificationsInput) (*listOutput, error) {
		list := service.ListInput{
			Status:  domain.Status(in.Status),
			Channel: domain.Channel(in.Channel),
			Cursor:  in.Cursor,
			Limit:   in.Limit,
		}
		if !in.From.IsZero() {
			list.From = &in.From
		}
		if !in.To.IsZero() {
			list.To = &in.To
		}
		page, err := deps.Notifications.List(ctx, list)
		if err != nil {
			return nil, problemFor(ctx, loggerFrom(ctx, deps.Logger), err)
		}
		items := make([]NotificationBody, len(page.Items))
		for i, n := range page.Items {
			items[i] = toBody(n)
		}
		return &listOutput{Body: ListBody{Items: items, NextCursor: page.NextCursor}}, nil
	})

	conflictSchema := registry.Schema(reflect.TypeFor[CancelConflictBody](), true, "CancelConflictBody")
	huma.Register(api, huma.Operation{
		OperationID: "cancel-notification",
		Method:      http.MethodPost,
		Path:        "/notifications/{id}/cancel",
		Summary:     "Cancel a queued notification",
		Description: "Cancels the notification while it is queued: waiting for its first attempt, for its channel's rate limit " +
			"or for its retry time. A notification that is already cancelled answers 200 again. One that a worker holds " +
			"(processing) or that has ended (sent, failed) answers 409 with its current status: its POST may already be on " +
			"its way. After a 200, no new POST goes out for it. An unknown ID gets 404, a malformed one 422.",
		Tags: []string{"Notifications"},
		Responses: map[string]*huma.Response{
			"409": {
				Description: "The notification is processing, sent or failed and can no longer be cancelled",
				Content:     map[string]*huma.MediaType{"application/problem+json": {Schema: conflictSchema}},
			},
		},
		Errors: []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *getNotificationInput) (*notificationOutput, error) {
		logger := loggerFrom(ctx, deps.Logger)
		n, err := deps.Notifications.Cancel(ctx, in.ID)
		if err != nil {
			return nil, problemFor(ctx, logger, err)
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "notification cancelled", slog.String("notification_id", n.ID))
		return &notificationOutput{Body: toBody(n)}, nil
	})
}

func marshalAccepted(n domain.Notification) ([]byte, error) {
	return json.Marshal(AcceptedBody{ID: n.ID, Status: string(n.Status)})
}

// problemFor maps a service error to an RFC 9457 response. Unexpected errors
// are logged and answered with a generic 500, so no internal detail leaks.
func problemFor(ctx context.Context, logger *slog.Logger, err error) error {
	var (
		verr     *domain.ValidationError
		conflict *domain.CancelConflictError
	)
	switch {
	case errors.As(err, &verr):
		details := make([]error, len(verr.Errors))
		for i, fe := range verr.Errors {
			details[i] = &huma.ErrorDetail{Location: "body." + fe.Field, Message: fe.Message}
		}
		return huma.Error422UnprocessableEntity("validation failed", details...)
	case errors.Is(err, domain.ErrIdempotencyConflict):
		return huma.Error422UnprocessableEntity("Idempotency-Key was already used with a different request",
			&huma.ErrorDetail{Location: "header." + IdempotencyKeyHeader, Message: "already used with a different request"})
	case errors.Is(err, domain.ErrNotFound):
		return huma.Error404NotFound("notification not found")
	case errors.Is(err, domain.ErrBatchNotFound):
		return huma.Error404NotFound("batch not found")
	case errors.Is(err, domain.ErrTemplateNotFound):
		return huma.Error404NotFound("template not found")
	case errors.Is(err, domain.ErrInvalidCursor):
		return huma.Error400BadRequest("cursor is not valid",
			&huma.ErrorDetail{Location: "query.cursor", Message: "is not the next_cursor of a previous page"})
	case errors.As(err, &conflict):
		return &CancelConflictBody{
			ErrorModel: huma.ErrorModel{
				Title:  http.StatusText(http.StatusConflict),
				Status: http.StatusConflict,
				Detail: "the notification is " + string(conflict.Status) + "; only a queued notification can be cancelled",
			},
			CurrentStatus: string(conflict.Status),
		}
	default:
		logger.LogAttrs(ctx, slog.LevelError, "request failed", slog.String("error", err.Error()))
		return huma.Error500InternalServerError("internal error")
	}
}
