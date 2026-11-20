package httpapi

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HealthcheckTimeout bounds the healthcheck subcommand's call (K31).
const HealthcheckTimeout = 2 * time.Second

// Healthcheck calls GET /readyz on 127.0.0.1:port and returns the exit status
// for `api healthcheck`: 0 on 200, otherwise 1. The runtime image has no shell
// or curl, so Compose runs this subcommand as the API's health check (K31).
func Healthcheck(ctx context.Context, port string, out io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, HealthcheckTimeout)
	defer cancel()

	url := "http://127.0.0.1:" + port + "/readyz"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		_, _ = fmt.Fprintf(out, "healthcheck: %v\n", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		_, _ = fmt.Fprintf(out, "healthcheck: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	if resp.StatusCode != http.StatusOK {
		_, _ = fmt.Fprintf(out, "healthcheck: /readyz answered %d\n", resp.StatusCode)
		return 1
	}
	_, _ = fmt.Fprintln(out, "healthcheck: ready")
	return 0
}
