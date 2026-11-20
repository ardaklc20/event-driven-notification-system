// Command worker claims queued notifications and delivers them to the
// provider (K2, K3, K6). Several copies can run against the same database:
//
//	docker compose up --scale worker=2
//
// On SIGTERM it stops claiming at once, lets the calls in flight finish and
// write their results, and exits within SHUTDOWN_BUDGET.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/config"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
	"github.com/ardaklc20/event-driven-notification-system/internal/worker"
)

func main() {
	os.Exit(run())
}

func run() int {
	// The worker ID goes on every line, the configuration error included (K21).
	id := worker.NewID()
	cfg, err := config.LoadWorker(env.Options{})
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(slog.String("worker_id", id)).
			Error("invalid configuration", "error", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel})).
		With(slog.String("worker_id", id))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database pool", "error", err)
		return 1
	}
	defer pool.Close()

	// The URL is not logged: a webhook.site URL holds its token.
	logger.Info("provider configured", slog.String("profile", string(cfg.ProviderProfile)),
		slog.String("http_timeout", cfg.HTTPTimeout.String()))
	client := provider.NewClient(cfg.ProviderURL, cfg.HTTPTimeout, cfg.InFlight)
	w := worker.New(postgres.NewStore(pool), client, logger, worker.Config{
		LeaseDuration:  cfg.LeaseDuration,
		ResultBudget:   cfg.ResultBudget,
		InFlight:       cfg.InFlight,
		IdleWait:       cfg.IdleWait,
		ShutdownBudget: cfg.ShutdownBudget,
	})

	// After the first signal, a second one terminates the process at once.
	go func() {
		<-ctx.Done()
		stop()
	}()
	if err := w.Run(ctx); err != nil {
		if !errors.Is(err, worker.ErrShutdownBudget) { // Run logged that one itself
			logger.Error("worker stopped with error", "error", err)
		}
		return 1
	}
	return 0
}
