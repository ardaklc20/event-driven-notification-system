// Command api runs the HTTP API. Subcommands:
//
//	api              serve the API
//	api healthcheck  call the local /readyz; exit 0 on 200, otherwise 1 (K31)
//	api openapi      print the generated OpenAPI document (K19)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/config"
	"github.com/ardaklc20/event-driven-notification-system/internal/httpapi"
	"github.com/ardaklc20/event-driven-notification-system/internal/postgres"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(runHealthcheck())
		case "openapi":
			os.Exit(runOpenAPI())
		default:
			fmt.Fprintf(os.Stderr, "unknown command %q\nusage: api [healthcheck | openapi]\n", os.Args[1])
			os.Exit(2)
		}
	}
	os.Exit(runServer())
}

func runServer() int {
	cfg, err := config.LoadAPI(env.Options{})
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("invalid configuration", "error", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("database pool", "error", err)
		return 1
	}
	defer pool.Close()

	readiness := &httpapi.Readiness{}
	handler := httpapi.New(httpapi.Deps{
		Logger:    logger,
		Readiness: readiness,
		Checks: []httpapi.Check{{
			Name: "database",
			Run:  func(ctx context.Context) error { return postgres.Ping(ctx, pool) },
		}},
	})
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		logger.Error("listen", "addr", cfg.HTTPAddr, "error", err)
		return 1
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("api listening", "addr", ln.Addr().String())

	select {
	case err := <-serveErr:
		logger.Error("server stopped unexpectedly", "error", err)
		return 1
	case <-ctx.Done():
	}
	stop() // a second signal now terminates the process at once

	// Readiness goes off first, so /readyz answers 503 while in-flight
	// requests finish (K6, K30).
	readiness.StartShutdown()
	logger.Info("shutdown started, readiness off", "timeout", cfg.ShutdownTimeout.String())

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("server shutdown", "error", err)
		return 1
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped with error", "error", err)
		return 1
	}
	pool.Close()
	logger.Info("server stopped")
	return 0
}

// runHealthcheck reads only the listen address, not the full configuration.
func runHealthcheck() int {
	cfg, err := config.LoadListen(env.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	port, err := cfg.Port()
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck: %v\n", err)
		return 1
	}
	return httpapi.Healthcheck(context.Background(), port, os.Stdout)
}

func runOpenAPI() int {
	doc, err := httpapi.OpenAPIYAML()
	if err != nil {
		fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
		return 1
	}
	if _, err := os.Stdout.Write(doc); err != nil {
		fmt.Fprintf(os.Stderr, "openapi: %v\n", err)
		return 1
	}
	return 0
}
