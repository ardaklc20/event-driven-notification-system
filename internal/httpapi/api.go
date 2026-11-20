// Package httpapi holds the HTTP layer: the standard library router (K16), the
// Huma operations that generate the OpenAPI document (K19) and the middleware.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"

	"github.com/ardaklc20/event-driven-notification-system/internal/service"
)

const (
	apiTitle   = "Notification System API"
	apiVersion = "1.0.0"
)

// Deps are what the HTTP layer needs from the program that runs it.
type Deps struct {
	Logger    *slog.Logger
	Readiness *Readiness
	// Checks run on every GET /readyz (K30).
	Checks        []Check
	Notifications *service.Notifications
}

// New returns the API's handler: the router with every operation registered,
// wrapped in the middleware.
func New(deps Deps) http.Handler {
	mux, _ := newAPI(deps)
	var h http.Handler = mux
	h = recoverPanics(deps.Logger, h)
	h = logRequests(deps.Logger, h)
	h = withCorrelationID(deps.Logger, h)
	return h
}

// OpenAPIYAML returns the generated OpenAPI 3.1 document. api/openapi.yaml is
// this output, committed (K19).
func OpenAPIYAML() ([]byte, error) {
	_, api := newAPI(Deps{
		Logger:    slog.New(slog.DiscardHandler),
		Readiness: &Readiness{},
	})
	return api.OpenAPI().YAML()
}

func newAPI(deps Deps) (*http.ServeMux, huma.API) {
	mux := http.NewServeMux()
	api := humago.New(mux, apiConfig())
	registerHealth(api, deps)
	registerNotifications(api, deps)
	return mux, api
}

func apiConfig() huma.Config {
	cfg := huma.DefaultConfig(apiTitle, apiVersion)
	cfg.Info.Description = "Event-driven notification system: queue SMS, email and push notifications and track their delivery."
	// The default create hook adds a `$schema` field to every response body and
	// a Link header; responses stay plain JSON without it.
	cfg.CreateHooks = nil
	cfg.Transformers = append(cfg.Transformers, dropEchoedValues)
	return cfg
}

// dropEchoedValues removes the `value` of every error detail. Huma copies the
// raw request body there when JSON cannot be parsed, and the offending value
// for schema errors; a request body is never echoed back (K19).
func dropEchoedValues(_ huma.Context, _ string, v any) (any, error) {
	if m, ok := v.(*huma.ErrorModel); ok {
		for _, d := range m.Errors {
			if d != nil {
				d.Value = nil
			}
		}
	}
	return v, nil
}
