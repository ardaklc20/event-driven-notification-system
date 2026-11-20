package worker

import "github.com/ardaklc20/event-driven-notification-system/internal/domain"

// Hooks stop a delivery at a chosen point to simulate a crash (slice 4).
type Hooks struct {
	// AfterClaim runs after the claim, before the provider call.
	AfterClaim func(domain.Claim)
	// BeforeWrite runs after the provider answered, before the result write.
	BeforeWrite func(domain.Claim)
}

// SetHooks installs h. Call it before Run.
func SetHooks(w *Worker, h Hooks) {
	w.hooks = hooks{afterClaim: h.AfterClaim, beforeWrite: h.BeforeWrite}
}
