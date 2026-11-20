package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/danielgtaylor/huma/v2"
)

// Readiness records whether the API is shutting down. Once shutdown has
// started, GET /readyz answers 503 (K6, K30).
type Readiness struct {
	shuttingDown atomic.Bool
}

// StartShutdown turns readiness off for good.
func (r *Readiness) StartShutdown() { r.shuttingDown.Store(true) }

// ShuttingDown reports whether StartShutdown has been called.
func (r *Readiness) ShuttingDown() bool { return r.shuttingDown.Load() }

// Check is one readiness check, such as the database ping.
type Check struct {
	Name string
	Run  func(context.Context) error
}

// LivenessOutput is the response of GET /healthz.
type LivenessOutput struct {
	Body LivenessBody
}

// LivenessBody reports that the process can answer.
type LivenessBody struct {
	Status string `json:"status" enum:"ok" doc:"Always ok while the process can answer"`
}

// ReadinessOutput is the 200 response of GET /readyz.
type ReadinessOutput struct {
	Body ReadinessBody
}

// ReadinessBody lists the result of each check.
type ReadinessBody struct {
	Status string            `json:"status" enum:"ready" doc:"ready when every check passed"`
	Checks map[string]string `json:"checks" doc:"Result of each check by name, for example {\"database\": \"pass\"}"`
}

func registerHealth(api huma.API, deps Deps) {
	huma.Register(api, huma.Operation{
		OperationID: "get-healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Liveness",
		Description: "Answers 200 whenever the process can answer. It checks no dependency.",
		Tags:        []string{"Health"},
	}, func(context.Context, *struct{}) (*LivenessOutput, error) {
		return &LivenessOutput{Body: LivenessBody{Status: "ok"}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-readyz",
		Method:      http.MethodGet,
		Path:        "/readyz",
		Summary:     "Readiness",
		Description: "Pings PostgreSQL with a short time limit. Answers 200 with each check's result, " +
			"or 503 naming the failing check. Answers 503 once shutdown has started.",
		Tags:   []string{"Health"},
		Errors: []int{http.StatusServiceUnavailable},
	}, func(ctx context.Context, _ *struct{}) (*ReadinessOutput, error) {
		return readyz(ctx, deps)
	})
}

func readyz(ctx context.Context, deps Deps) (*ReadinessOutput, error) {
	if deps.Readiness.ShuttingDown() {
		return nil, huma.Error503ServiceUnavailable("shutting down")
	}
	results := make(map[string]string, len(deps.Checks))
	var failed []error
	for _, check := range deps.Checks {
		if err := check.Run(ctx); err != nil {
			// The cause stays in the log; the response only names the check.
			deps.Logger.LogAttrs(ctx, slog.LevelWarn, "readiness check failed",
				slog.String("check", check.Name), slog.String("error", err.Error()))
			results[check.Name] = "fail"
			failed = append(failed, &huma.ErrorDetail{Location: "checks." + check.Name, Message: "check failed"})
			continue
		}
		results[check.Name] = "pass"
	}
	if len(failed) > 0 {
		return nil, huma.Error503ServiceUnavailable("not ready", failed...)
	}
	return &ReadinessOutput{Body: ReadinessBody{Status: "ready", Checks: results}}, nil
}
