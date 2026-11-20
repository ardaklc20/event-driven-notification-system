package httpapi

import (
	"context"
	"net/http"
)

// WSHooks stop a WebSocket connection at fixed points in tests.
type WSHooks struct {
	// AfterUpgrade runs after the upgrade and before the registration.
	AfterUpgrade func(r *http.Request)
	// BeforeSnapshot runs after the registration and before the snapshot read.
	BeforeSnapshot func(r *http.Request)
	// BeforeFirstWrite runs before the snapshot is written; ctx ends with the
	// subscription.
	BeforeFirstWrite func(ctx context.Context, r *http.Request)
}

// SetWSHooks installs h on d.
func SetWSHooks(d *WebSocketDeps, h WSHooks) {
	d.hooks = wsHooks{afterUpgrade: h.AfterUpgrade, beforeSnapshot: h.BeforeSnapshot, beforeFirstWrite: h.BeforeFirstWrite}
}
