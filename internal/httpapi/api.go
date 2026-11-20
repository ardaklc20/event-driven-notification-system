// Package httpapi holds the HTTP layer: the standard library router (K16), the
// Huma operations that generate the OpenAPI document (K19) and the middleware.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
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
	Checks []Check
}

// New returns the API's handler: the router with every operation registered,
// wrapped in the middleware.
func New(deps Deps) http.Handler {
	mux, _ := newAPI(deps)
	return logRequests(deps.Logger, mux)
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
	return mux, api
}

func apiConfig() huma.Config {
	cfg := huma.DefaultConfig(apiTitle, apiVersion)
	cfg.Info.Description = "Event-driven notification system: queue SMS, email and push notifications and track their delivery."
	// The default create hook adds a `$schema` field to every response body and
	// a Link header; responses stay plain JSON without it.
	cfg.CreateHooks = nil
	return cfg
}
