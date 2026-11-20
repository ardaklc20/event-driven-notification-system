package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/service"
)

// BatchRequest is the body of POST /notifications/batch.
type BatchRequest struct {
	Notifications []NotificationRequest `json:"notifications" minItems:"1" maxItems:"1000" doc:"1 to 1000 notifications; each has its own channel and priority and follows the rules of POST /notifications"`
}

func (r BatchRequest) toDomain() []domain.NewNotification {
	items := make([]domain.NewNotification, len(r.Notifications))
	for i, n := range r.Notifications {
		items[i] = n.toDomain()
	}
	return items
}

type createBatchInput struct {
	IdempotencyKey string `header:"Idempotency-Key" doc:"Optional. One key covers the whole batch. Sent once, 1 to 128 characters from A–Z, a–z, 0–9, '.', '_' and '-'. A repeat with the same key and the same batch returns the first 202 response unchanged; the same key with a different request, or a key used for a single create, gets 422."`
	Body           BatchRequest
}

// Resolve checks the Idempotency-Key header.
func (in *createBatchInput) Resolve(ctx huma.Context) []error {
	return resolveIdempotencyKey(ctx)
}

// BatchAcceptedBody is the 202 response of POST /notifications/batch.
type BatchAcceptedBody struct {
	BatchID         string   `json:"batch_id" format:"uuid" doc:"Batch ID (UUIDv7)"`
	NotificationIDs []string `json:"notification_ids" doc:"The IDs in request order: notification_ids[i] belongs to notifications[i]"`
}

type getBatchInput struct {
	ID     string `path:"id" format:"uuid" doc:"Batch ID"`
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Page size; outside 1 to 200 gets 422"`
	Cursor string `query:"cursor" doc:"The next_cursor of the previous page, unchanged"`
}

// BatchCounts holds the batch's notifications per status; a status with no
// notification is 0, never missing.
type BatchCounts struct {
	Queued     int `json:"queued" minimum:"0"`
	Processing int `json:"processing" minimum:"0"`
	Sent       int `json:"sent" minimum:"0"`
	Failed     int `json:"failed" minimum:"0"`
	Cancelled  int `json:"cancelled" minimum:"0"`
}

// BatchBody is a batch as GET /batches/{id} returns it.
type BatchBody struct {
	ID        string             `json:"id" format:"uuid"`
	ItemCount int                `json:"item_count" minimum:"1" maximum:"1000"`
	CreatedAt time.Time          `json:"created_at"`
	Counts    BatchCounts        `json:"counts" doc:"Read in the same snapshot as this page's items"`
	Items     []NotificationBody `json:"items" doc:"The batch's notifications, newest first: created_at descending, then ID descending"`
	// NextCursor reads the next page of items.
	NextCursor string `json:"next_cursor,omitempty" doc:"Pass as cursor to read the next page; absent on the last page"`
}

type batchOutput struct {
	Body BatchBody
}

func registerBatches(api huma.API, deps Deps) {
	maxBody := deps.BatchMaxBodyBytes
	if maxBody == 0 {
		maxBody = DefaultBatchMaxBodyBytes
	}
	acceptedSchema := api.OpenAPI().Components.Schemas.Schema(reflect.TypeFor[BatchAcceptedBody](), true, "BatchAcceptedBody")

	huma.Register(api, huma.Operation{
		OperationID: "create-batch",
		Method:      http.MethodPost,
		Path:        "/notifications/batch",
		Summary:     "Create a batch of notifications",
		Description: "Saves 1 to 1000 notifications all or nothing: if any item is invalid, the answer is 422 with each error's " +
			"item index and field, and nothing is saved. Channels and priorities may be mixed. The body may be at most " +
			"16 MiB by default (BATCH_MAX_BODY_BYTES), so a batch of large items can get 413 with fewer than 1000 items.",
		Tags:          []string{"Batches"},
		DefaultStatus: http.StatusAccepted,
		MaxBodyBytes:  maxBody,
		Responses: map[string]*huma.Response{
			"202": {
				Description: "Accepted; every notification is queued",
				Content:     map[string]*huma.MediaType{"application/json": {Schema: acceptedSchema}},
			},
		},
		Errors: []int{http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *createBatchInput) (*rawJSONOutput, error) {
		logger := loggerFrom(ctx, deps.Logger)
		res, err := deps.Notifications.CreateBatch(ctx, service.BatchInput{
			Items:         in.Body.toDomain(),
			Key:           in.IdempotencyKey,
			CorrelationID: correlationIDFrom(ctx),
		}, marshalBatchAccepted)
		if err != nil {
			return nil, problemFor(ctx, logger, err)
		}
		var accepted BatchAcceptedBody
		_ = json.Unmarshal(res.Body, &accepted) // the stored bytes are this type; only logged
		logger.LogAttrs(ctx, slog.LevelInfo, "batch accepted",
			slog.String("batch_id", accepted.BatchID),
			slog.Int("items", len(in.Body.Notifications)),
			slog.Bool("replayed", res.Replayed),
		)
		return &rawJSONOutput{Status: res.Status, ContentType: "application/json", Body: res.Body}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-batch",
		Method:      http.MethodGet,
		Path:        "/batches/{id}",
		Summary:     "Get a batch's status",
		Description: "Returns the batch's notifications counted by status, every status included, and one page of its " +
			"notifications in the listing order; follow next_cursor for the rest. The counts and the page are read in " +
			"one snapshot. An unknown ID gets 404, a malformed one 422, a cursor no page produced 400.",
		Tags:   []string{"Batches"},
		Errors: []int{http.StatusBadRequest, http.StatusNotFound, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *getBatchInput) (*batchOutput, error) {
		res, err := deps.Notifications.GetBatch(ctx, in.ID, in.Cursor, in.Limit)
		if err != nil {
			return nil, problemFor(ctx, loggerFrom(ctx, deps.Logger), err)
		}
		b := res.Batch
		items := make([]NotificationBody, len(res.Page.Items))
		for i, n := range res.Page.Items {
			items[i] = toBody(n)
		}
		return &batchOutput{Body: BatchBody{
			ID:        b.ID,
			ItemCount: b.ItemCount,
			CreatedAt: b.CreatedAt,
			Counts: BatchCounts{
				Queued:     b.Counts[domain.StatusQueued],
				Processing: b.Counts[domain.StatusProcessing],
				Sent:       b.Counts[domain.StatusSent],
				Failed:     b.Counts[domain.StatusFailed],
				Cancelled:  b.Counts[domain.StatusCancelled],
			},
			Items:      items,
			NextCursor: res.Page.NextCursor,
		}}, nil
	})
}

func marshalBatchAccepted(batchID string, ids []string) ([]byte, error) {
	return json.Marshal(BatchAcceptedBody{BatchID: batchID, NotificationIDs: ids})
}
