// Command migrate applies the embedded SQL migrations (K18):
//
//	migrate up
//
// It logs each applied version as JSON and exits with 1 on any error. A failed
// migration is rolled back on its own; the ones before it stay applied.
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/caarlos0/env/v11"
	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/pressly/goose/v3"

	"github.com/ardaklc20/event-driven-notification-system/internal/config"
	"github.com/ardaklc20/event-driven-notification-system/migrations"
)

func main() {
	os.Exit(run())
}

func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) != 2 || os.Args[1] != "up" {
		logger.Error("usage: migrate up")
		return 2
	}
	cfg, err := config.LoadMigrate(env.Options{})
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		return 1
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		logger.Error("open database", "error", err)
		return 1
	}
	provider, err := migrations.NewProvider(db)
	if err != nil {
		_ = db.Close()
		logger.Error("goose provider", "error", err)
		return 1
	}
	defer func() { _ = provider.Close() }() // also closes db

	results, err := provider.Up(ctx)
	if err != nil {
		var partial *goose.PartialError
		if errors.As(err, &partial) {
			for _, r := range partial.Applied {
				logApplied(logger, r)
			}
			logger.Error("migration failed",
				"version", partial.Failed.Source.Version,
				"file", partial.Failed.Source.Path,
				"error", partial.Err.Error())
		} else {
			logger.Error("migrate up failed", "error", err.Error())
		}
		return 1
	}
	for _, r := range results {
		logApplied(logger, r)
	}
	if len(results) == 0 {
		logger.Info("no migrations to apply")
	} else {
		logger.Info("migrations complete", "applied", len(results))
	}
	return 0
}

func logApplied(logger *slog.Logger, r *goose.MigrationResult) {
	logger.Info("migration applied",
		"version", r.Source.Version,
		"file", r.Source.Path,
		"duration", r.Duration.String())
}
