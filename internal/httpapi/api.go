// Package httpapi holds the HTTP layer: the standard library router (K16), the
// Huma operations that generate the OpenAPI document (K19) and the middleware.
package httpapi

import (
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/ardaklc20/event-driven-notification-system/internal/service"
	"github.com/ardaklc20/event-driven-notification-system/internal/tracing"
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
	Metrics       *service.Metrics
	Templates     *service.Templates
	// BatchMaxBodyBytes is the largest batch body; 0 means
	// DefaultBatchMaxBodyBytes (K12).
	BatchMaxBodyBytes int64
	// WebSocket configures the real-time status endpoints (K24, K25); they
	// are registered only when it has a Hub.
	WebSocket WebSocketDeps
	// Tracing, when enabled, wraps the API in otelhttp's server middleware
	// (K27). Nil or disabled: no spans, and no trace context is stored.
	Tracing *tracing.Provider
}

// DefaultBatchMaxBodyBytes is the default batch body limit, 16 MiB (K12).
const DefaultBatchMaxBodyBytes = 16 << 20

// New returns the API's handler: the router with every operation registered,
// wrapped in the middleware.
func New(deps Deps) http.Handler {
	mux, _ := newAPI(deps)
	var h http.Handler = mux
	h = recoverPanics(deps.Logger, h)
	h = logRequests(deps.Logger, h)
	h = withCorrelationID(deps.Logger, h)
	if deps.Tracing.Enabled() {
		// Outermost, so the server span covers the whole request; a
		// client's traceparent makes it a child of the client's span.
		h = otelhttp.NewHandler(h, "api",
			otelhttp.WithTracerProvider(deps.Tracing.TracerProvider()),
			otelhttp.WithPropagators(deps.Tracing.Propagator()),
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string { return r.Method + " " + r.URL.Path }),
			// Probes every few seconds would bury the requests (K30).
			otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" && r.URL.Path != "/readyz" }),
		)
	}
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
	registerBatches(api, deps)
	registerMetrics(api, deps)
	registerTemplates(api, deps)
	// Plain handlers on the same mux: OpenAPI does not describe WebSockets (K25).
	registerWebSockets(mux, deps)
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
