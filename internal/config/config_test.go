package config_test

import (
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/config"
	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
)

const testURL = "postgres://u:p@localhost:5432/db"

func TestAPIConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := config.LoadAPI(env.Options{Environment: map[string]string{"DATABASE_URL": testURL}})
		if err != nil {
			t.Fatalf("LoadAPI: %v", err)
		}
		if cfg.DatabaseURL != testURL {
			t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
		}
		if cfg.HTTPAddr != ":8080" {
			t.Errorf("HTTPAddr = %q, want :8080", cfg.HTTPAddr)
		}
		if cfg.LogLevel != slog.LevelInfo {
			t.Errorf("LogLevel = %v, want INFO", cfg.LogLevel)
		}
		if cfg.ShutdownTimeout != 10*time.Second {
			t.Errorf("ShutdownTimeout = %s, want 10s", cfg.ShutdownTimeout)
		}
		if cfg.Limits() != domain.DefaultLimits() {
			t.Errorf("Limits = %+v, want %+v", cfg.Limits(), domain.DefaultLimits())
		}
	})

	t.Run("explicit values", func(t *testing.T) {
		cfg, err := config.LoadAPI(env.Options{Environment: map[string]string{
			"DATABASE_URL":     testURL,
			"HTTP_ADDR":        "127.0.0.1:9090",
			"LOG_LEVEL":        "debug",
			"SHUTDOWN_TIMEOUT": "3s",
		}})
		if err != nil {
			t.Fatalf("LoadAPI: %v", err)
		}
		if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second {
			t.Errorf("got %+v", cfg)
		}
	})

	t.Run("missing DATABASE_URL", func(t *testing.T) {
		_, err := config.LoadAPI(env.Options{Environment: map[string]string{}})
		var notSet env.VarIsNotSetError
		if !errors.As(err, &notSet) || notSet.Key != "DATABASE_URL" {
			t.Fatalf("err = %v, want VarIsNotSetError for DATABASE_URL", err)
		}
	})

	t.Run("empty DATABASE_URL", func(t *testing.T) {
		_, err := config.LoadAPI(env.Options{Environment: map[string]string{"DATABASE_URL": ""}})
		var empty env.EmptyVarError
		if !errors.As(err, &empty) || empty.Key != "DATABASE_URL" {
			t.Fatalf("err = %v, want EmptyVarError for DATABASE_URL", err)
		}
	})

	t.Run("several errors in one result", func(t *testing.T) {
		_, err := config.LoadAPI(env.Options{Environment: map[string]string{
			"LOG_LEVEL":        "loud",
			"SHUTDOWN_TIMEOUT": "soon",
		}})
		var agg env.AggregateError
		if !errors.As(err, &agg) {
			t.Fatalf("err = %v, want AggregateError", err)
		}
		if len(agg.Errors) != 3 {
			t.Fatalf("got %d errors, want 3 (DATABASE_URL, LOG_LEVEL, SHUTDOWN_TIMEOUT): %v", len(agg.Errors), err)
		}
	})

	t.Run("validation", func(t *testing.T) {
		for name, vars := range map[string]map[string]string{
			"address without port": {"DATABASE_URL": testURL, "HTTP_ADDR": "localhost"},
			"zero shutdown":        {"DATABASE_URL": testURL, "SHUTDOWN_TIMEOUT": "0s"},
			"zero sms limit":       {"DATABASE_URL": testURL, "SMS_CONTENT_LIMIT": "0"},
			"negative push limit":  {"DATABASE_URL": testURL, "PUSH_CONTENT_LIMIT": "-1"},
		} {
			if _, err := config.LoadAPI(env.Options{Environment: vars}); err == nil {
				t.Errorf("%s: LoadAPI accepted %v", name, vars)
			}
		}
	})
}

func TestListenConfig(t *testing.T) {
	cfg, err := config.LoadListen(env.Options{Environment: map[string]string{}})
	if err != nil {
		t.Fatalf("LoadListen: %v", err)
	}
	if port, _ := cfg.Port(); port != "8080" {
		t.Errorf("port = %q, want 8080", port)
	}
	cfg, err = config.LoadListen(env.Options{Environment: map[string]string{"HTTP_ADDR": "0.0.0.0:9000"}})
	if err != nil {
		t.Fatalf("LoadListen: %v", err)
	}
	if port, _ := cfg.Port(); port != "9000" {
		t.Errorf("port = %q, want 9000", port)
	}
}
