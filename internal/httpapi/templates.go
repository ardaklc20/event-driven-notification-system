package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

// TemplateRequest is the body of POST /templates (K33, K47).
type TemplateRequest struct {
	Name    string `json:"name" minLength:"1" example:"welcome" doc:"1 to 100 Unicode code points, no NUL. Not unique: created_at tells templates with the same name apart."`
	Channel string `json:"channel" enum:"sms,email,push" example:"sms" doc:"The channel every notification made from this template must use"`
	Body    string `json:"body" minLength:"1" example:"Hello {{first_name}}, your code is {{code}}" doc:"1 to 10000 Unicode code points, no NUL. {{name}} is a placeholder; a name matches [A-Za-z_][A-Za-z0-9_]* with no spaces. Single braces are plain text."`
}

func (r TemplateRequest) toDomain() domain.NewTemplate {
	return domain.NewTemplate{Name: r.Name, Channel: domain.Channel(r.Channel), Body: r.Body}
}

// Resolve runs the K47 rules after Huma's schema validation; missing fields
// and unknown channels are left to the schema, which already reports them.
func (r *TemplateRequest) Resolve(_ huma.Context, prefix *huma.PathBuffer) []error {
	var errs []error
	for _, fe := range domain.ValidateTemplate(r.toDomain()) {
		if fe.Code == domain.CodeRequired || fe.Code == domain.CodeUnknownValue {
			continue
		}
		errs = append(errs, &huma.ErrorDetail{Location: prefix.With(fe.Field), Message: fe.Message})
	}
	return errs
}

// TemplateBody is a stored template.
type TemplateBody struct {
	ID        string    `json:"id" format:"uuid" doc:"Template ID (UUIDv7)"`
	Name      string    `json:"name"`
	Channel   string    `json:"channel" enum:"sms,email,push"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

func toTemplateBody(t domain.Template) TemplateBody {
	return TemplateBody{ID: t.ID, Name: t.Name, Channel: string(t.Channel), Body: t.Body, CreatedAt: t.CreatedAt}
}

type createTemplateInput struct {
	Body TemplateRequest
}

type templateOutput struct {
	Body TemplateBody
}

type getTemplateInput struct {
	ID string `path:"id" format:"uuid" doc:"Template ID"`
}

type listTemplatesInput struct {
	Limit  int    `query:"limit" default:"50" minimum:"1" maximum:"200" doc:"Page size; outside 1 to 200 gets 422"`
	Cursor string `query:"cursor" doc:"The next_cursor of the previous page, unchanged"`
}

// TemplateListBody is one page of GET /templates.
type TemplateListBody struct {
	Items      []TemplateBody `json:"items" doc:"Newest first: created_at descending, then ID descending"`
	NextCursor string         `json:"next_cursor,omitempty" doc:"Pass as cursor to read the next page; absent on the last page"`
}

type templateListOutput struct {
	Body TemplateListBody
}

func registerTemplates(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID:   "create-template",
		Method:        http.MethodPost,
		Path:          "/templates",
		Summary:       "Create a template",
		Description:   "Stores a template with {{name}} placeholders. Templates cannot be changed or deleted; a changed message is a new template. Malformed JSON gets 400, a broken rule 422.",
		Tags:          []string{"Templates"},
		DefaultStatus: http.StatusCreated,
		Errors:        []int{http.StatusBadRequest, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *createTemplateInput) (*templateOutput, error) {
		logger := loggerFrom(ctx, deps.Logger)
		t, err := deps.Templates.Create(ctx, in.Body.toDomain())
		if err != nil {
			return nil, problemFor(ctx, logger, err)
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "template created",
			slog.String("template_id", t.ID), slog.String("channel", string(t.Channel)))
		return &templateOutput{Body: toTemplateBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-template",
		Method:      http.MethodGet,
		Path:        "/templates/{id}",
		Summary:     "Get a template",
		Description: "A malformed ID gets 422, an unknown one 404.",
		Tags:        []string{"Templates"},
		Errors:      []int{http.StatusNotFound, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *getTemplateInput) (*templateOutput, error) {
		t, err := deps.Templates.Get(ctx, in.ID)
		if err != nil {
			return nil, problemFor(ctx, loggerFrom(ctx, deps.Logger), err)
		}
		return &templateOutput{Body: toTemplateBody(t)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-templates",
		Method:      http.MethodGet,
		Path:        "/templates",
		Summary:     "List templates",
		Description: "Lists templates newest first. Follow next_cursor to read further pages. A cursor that was not produced by a previous page gets 400.",
		Tags:        []string{"Templates"},
		Errors:      []int{http.StatusBadRequest, http.StatusUnprocessableEntity},
	}, func(ctx context.Context, in *listTemplatesInput) (*templateListOutput, error) {
		page, err := deps.Templates.List(ctx, in.Cursor, in.Limit)
		if err != nil {
			return nil, problemFor(ctx, loggerFrom(ctx, deps.Logger), err)
		}
		items := make([]TemplateBody, len(page.Items))
		for i, t := range page.Items {
			items[i] = toTemplateBody(t)
		}
		return &templateListOutput{Body: TemplateListBody{Items: items, NextCursor: page.NextCursor}}, nil
	})
}
