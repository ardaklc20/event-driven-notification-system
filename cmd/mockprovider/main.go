// Command mockprovider runs the local provider Compose uses by default (K14):
//
//	POST /      202 {messageId, status: "accepted", timestamp} after MOCK_DELAY;
//	            400 for a body that is not {to, channel, content}
//	GET /stats  arrivals per channel and the most in any one-second window
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/config"
	"github.com/ardaklc20/event-driven-notification-system/internal/mockprovider"
)

const shutdownTimeout = 5 * time.Second

func main() {
	os.Exit(run())
}

func run() int {
	cfg, err := config.LoadMockProvider(env.Options{})
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("invalid configuration", "error", err)
		return 1
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Handler:           mockprovider.New(cfg.Delay, logger).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Long enough for the configured delay.
		WriteTimeout: cfg.Delay + 30*time.Second,
		IdleTimeout:  60 * time.Second,
		ErrorLog:     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.HTTPAddr)
	if err != nil {
		logger.Error("listen", "addr", cfg.HTTPAddr, "error", err)
		return 1
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	logger.Info("mock provider listening", slog.String("addr", ln.Addr().String()),
		slog.String("delay", cfg.Delay.String()))

	select {
	case err := <-serveErr:
		logger.Error("server stopped unexpectedly", "error", err)
		return 1
	case <-ctx.Done():
	}
	stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("shutdown", "error", err)
		return 1
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped with error", "error", err)
		return 1
	}
	logger.Info("mock provider stopped")
	return 0
}
