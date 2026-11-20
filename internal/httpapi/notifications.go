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
	Content   string `json:"content" minLength:"1" example:"Your code is 123456" doc:"At most 160 (sms), 10000 (email) or 1000 (push) Unicode code points by default; the limits are configurable and apply to new notifications only."`
	Priority  string `json:"priority,omitempty" enum:"high,normal,low" default:"normal"`
}

func (r NotificationRequest) toDomain() domain.NewNotification {
	return domain.NewNotification{
		Recipient: r.Recipient,
		Channel:   domain.Channel(r.Channel),
		Content:   r.Content,
		Priority:  domain.Priority(r.Priority),
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
		if fe.Code == domain.CodeRequired || fe.Code == domain.CodeUnknownValue {
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

// Resolve checks every raw value of the Idempotency-Key header, so a repeated
// header is seen too. A bad key gets 400 and nothing is written (K10).
func (in *createNotificationInput) Resolve(ctx huma.Context) []error {
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
	ID        string    `json:"id" format:"uuid"`
	Recipient string    `json:"recipient"`
	Channel   string    `json:"channel" enum:"sms,email,push"`
	Content   string    `json:"content"`
	Priority  string    `json:"priority" enum:"high,normal,low"`
	Status    string    `json:"status" enum:"queued,processing,sent,failed,cancelled"`
	Attempts  int       `json:"attempts" minimum:"0" doc:"Attempts started so far; an attempt counts when a worker claims the notification"`
	LastError *string   `json:"last_error" nullable:"true" doc:"The last delivery error, or null"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type notificationOutput struct {
	Body NotificationBody
}

type getNotificationInput struct {
	ID string `path:"id" format:"uuid" doc:"Notification ID"`
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
		return &notificationOutput{Body: NotificationBody{
			ID:        n.ID,
			Recipient: n.Recipient,
			Channel:   string(n.Channel),
			Content:   n.Content,
			Priority:  string(n.Priority),
			Status:    string(n.Status),
			Attempts:  n.Attempts,
			LastError: n.LastError,
			CreatedAt: n.CreatedAt,
			UpdatedAt: n.UpdatedAt,
		}}, nil
	})
}

func marshalAccepted(n domain.Notification) ([]byte, error) {
	return json.Marshal(AcceptedBody{ID: n.ID, Status: string(n.Status)})
}

// problemFor maps a service error to an RFC 9457 response. Unexpected errors
// are logged and answered with a generic 500, so no internal detail leaks.
func problemFor(ctx context.Context, logger *slog.Logger, err error) error {
	var verr *domain.ValidationError
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
	default:
		logger.LogAttrs(ctx, slog.LevelError, "request failed", slog.String("error", err.Error()))
		return huma.Error500InternalServerError("internal error")
	}
}
