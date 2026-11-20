package config_test

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/config"
	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
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
		if cfg.BatchMaxBodyBytes != 16<<20 {
			t.Errorf("BatchMaxBodyBytes = %d, want 16 MiB", cfg.BatchMaxBodyBytes)
		}
	})

	t.Run("explicit values", func(t *testing.T) {
		cfg, err := config.LoadAPI(env.Options{Environment: map[string]string{
			"DATABASE_URL":         testURL,
			"HTTP_ADDR":            "127.0.0.1:9090",
			"LOG_LEVEL":            "debug",
			"SHUTDOWN_TIMEOUT":     "3s",
			"BATCH_MAX_BODY_BYTES": "1048576",
		}})
		if err != nil {
			t.Fatalf("LoadAPI: %v", err)
		}
		if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.LogLevel != slog.LevelDebug || cfg.ShutdownTimeout != 3*time.Second ||
			cfg.BatchMaxBodyBytes != 1<<20 {
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
			"zero batch body":      {"DATABASE_URL": testURL, "BATCH_MAX_BODY_BYTES": "0"},
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

const testProviderURL = "http://mockprovider:8081/"

func TestWorkerConfig(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		cfg, err := config.LoadWorker(env.Options{Environment: map[string]string{
			"DATABASE_URL": testURL,
			"PROVIDER_URL": testProviderURL,
		}})
		if err != nil {
			t.Fatalf("LoadWorker: %v", err)
		}
		want := config.Worker{
			DatabaseURL:      testURL,
			ProviderURL:      testProviderURL,
			ProviderProfile:  provider.ProfileStandard,
			HTTPTimeout:      5 * time.Second,
			ResultBudget:     5 * time.Second,
			LeaseDuration:    30 * time.Second,
			InFlight:         30,
			IdleWait:         200 * time.Millisecond,
			ShutdownBudget:   10 * time.Second,
			LogLevel:         slog.LevelInfo,
			MaxAttempts:      5,
			BackoffBase:      2 * time.Second,
			BackoffCap:       60 * time.Second,
			RecoveryInterval: 5 * time.Second,
			RecoveryBatch:    100,
			PermitInterval:   11 * time.Millisecond,
			PermitGrace:      100 * time.Millisecond,

			BreakerThreshold:   5,
			BreakerCooldown:    30 * time.Second,
			BreakerMaxCooldown: 5 * time.Minute,
		}
		if cfg != want {
			t.Errorf("got %+v\nwant %+v", cfg, want)
		}
	})

	t.Run("webhook.site profile", func(t *testing.T) {
		cfg, err := config.LoadWorker(env.Options{Environment: map[string]string{
			"DATABASE_URL":     testURL,
			"PROVIDER_URL":     "https://webhook.site/0b6b1a3c-0000-4000-8000-000000000000",
			"PROVIDER_PROFILE": "webhook_site_free",
		}})
		if err != nil || cfg.ProviderProfile != provider.ProfileWebhookSiteFree {
			t.Fatalf("profile = %q, err = %v", cfg.ProviderProfile, err)
		}
	})

	t.Run("missing PROVIDER_URL", func(t *testing.T) {
		_, err := config.LoadWorker(env.Options{Environment: map[string]string{"DATABASE_URL": testURL}})
		var notSet env.VarIsNotSetError
		if !errors.As(err, &notSet) || notSet.Key != "PROVIDER_URL" {
			t.Fatalf("err = %v, want VarIsNotSetError for PROVIDER_URL", err)
		}
	})

	t.Run("empty PROVIDER_URL", func(t *testing.T) {
		_, err := config.LoadWorker(env.Options{Environment: map[string]string{"DATABASE_URL": testURL, "PROVIDER_URL": ""}})
		var empty env.EmptyVarError
		if !errors.As(err, &empty) || empty.Key != "PROVIDER_URL" {
			t.Fatalf("err = %v, want EmptyVarError for PROVIDER_URL", err)
		}
	})

	t.Run("rejected values", func(t *testing.T) {
		for name, extra := range map[string]map[string]string{
			"unknown profile":                     {"PROVIDER_PROFILE": "webhook_site"},
			"lease shorter than timeout + budget": {"LEASE_DURATION": "9s", "HTTP_TIMEOUT": "5s", "RESULT_BUDGET": "5s"},
			"relative provider URL":               {"PROVIDER_URL": "mockprovider:8081"},
			"provider URL with another scheme":    {"PROVIDER_URL": "ftp://mockprovider/"},
			"zero in-flight calls":                {"IN_FLIGHT": "0"},
			"zero idle wait":                      {"IDLE_WAIT": "0s"},
			"zero shutdown budget":                {"SHUTDOWN_BUDGET": "0s"},
			"negative HTTP timeout":               {"HTTP_TIMEOUT": "-1s"},
			"zero recovery interval":              {"RECOVERY_INTERVAL": "0s"},
			"recovery batch below 1":              {"RECOVERY_BATCH": "0"},
			"zero attempts":                       {"MAX_ATTEMPTS": "0"},
			"backoff cap below base":              {"BACKOFF_BASE": "10s", "BACKOFF_CAP": "5s"},
			"negative backoff base":               {"BACKOFF_BASE": "-1s"},
			"breaker threshold below 1":           {"BREAKER_THRESHOLD": "0"},
			"zero breaker cooldown":               {"BREAKER_COOLDOWN": "0s"},
			"breaker cooldown above its maximum":  {"BREAKER_COOLDOWN": "10m", "BREAKER_MAX_COOLDOWN": "5m"},
			"test-only shared-quota profile":      {"PROVIDER_PROFILE": "shared_quota_test"},
		} {
			vars := map[string]string{"DATABASE_URL": testURL, "PROVIDER_URL": testProviderURL}
			for k, v := range extra {
				vars[k] = v
			}
			if _, err := config.LoadWorker(env.Options{Environment: vars}); err == nil {
				t.Errorf("%s: LoadWorker accepted %v", name, extra)
			}
		}
	})

	t.Run("breaker cooldown equal to its maximum", func(t *testing.T) {
		cfg, err := config.LoadWorker(env.Options{Environment: map[string]string{
			"DATABASE_URL": testURL, "PROVIDER_URL": testProviderURL,
			"BREAKER_THRESHOLD": "1", "BREAKER_COOLDOWN": "2s", "BREAKER_MAX_COOLDOWN": "2s",
		}})
		if err != nil {
			t.Fatalf("LoadWorker: %v", err)
		}
		want := domain.BreakerPolicy{Threshold: 1, Cooldown: 2 * time.Second, MaxCooldown: 2 * time.Second}
		if got := cfg.Breaker(); got != want {
			t.Errorf("Breaker() = %+v, want %+v", got, want)
		}
	})

	t.Run("lease equal to timeout + budget", func(t *testing.T) {
		if _, err := config.LoadWorker(env.Options{Environment: map[string]string{
			"DATABASE_URL": testURL, "PROVIDER_URL": testProviderURL,
			"LEASE_DURATION": "10s", "HTTP_TIMEOUT": "5s", "RESULT_BUDGET": "5s",
		}}); err != nil {
			t.Fatalf("LoadWorker: %v", err)
		}
	})

	t.Run("provider URL not repeated in the error", func(t *testing.T) {
		_, err := config.LoadWorker(env.Options{Environment: map[string]string{
			"DATABASE_URL": testURL, "PROVIDER_URL": "secret-token-path",
		}})
		if err == nil || strings.Contains(err.Error(), "secret-token-path") {
			t.Fatalf("err = %v, want an error that does not repeat the URL", err)
		}
	})
}

func TestMockProviderConfig(t *testing.T) {
	cfg, err := config.LoadMockProvider(env.Options{Environment: map[string]string{}})
	if err != nil || cfg.HTTPAddr != ":8081" || cfg.Delay != 0 {
		t.Fatalf("defaults: %+v, err %v", cfg, err)
	}
	cfg, err = config.LoadMockProvider(env.Options{Environment: map[string]string{"MOCK_DELAY": "3s"}})
	if err != nil || cfg.Delay != 3*time.Second {
		t.Fatalf("MOCK_DELAY=3s: %+v, err %v", cfg, err)
	}
	if _, err := config.LoadMockProvider(env.Options{Environment: map[string]string{"MOCK_DELAY": "-1s"}}); err == nil {
		t.Fatal("negative MOCK_DELAY accepted")
	}
}

// TestPermitConfig: Validate accepts a permit interval and grace only when
// 100 × PERMIT_INTERVAL − PERMIT_GRACE is at least one second, and a grace
// only when it is positive (K42).
func TestPermitConfig(t *testing.T) {
	for _, tt := range []struct {
		interval, grace string
		ok              bool
	}{
		{"11ms", "100ms", true},  // 1100 − 100 = 1000 ms
		{"10ms", "100ms", false}, // 1000 − 100 = 900 ms
		{"11ms", "0s", false},    // grace not positive
		{"11ms", "-1ms", false},  // grace not positive
		{"12ms", "200ms", true},  // 1200 − 200 = 1000 ms
		{"0s", "100ms", false},
	} {
		_, err := config.LoadWorker(env.Options{Environment: map[string]string{
			"DATABASE_URL": testURL, "PROVIDER_URL": testProviderURL,
			"PERMIT_INTERVAL": tt.interval, "PERMIT_GRACE": tt.grace,
		}})
		if (err == nil) != tt.ok {
			t.Errorf("PERMIT_INTERVAL=%s PERMIT_GRACE=%s: err %v, want accepted=%v", tt.interval, tt.grace, err, tt.ok)
		}
	}
}
