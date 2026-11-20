// Package config reads each program's settings from environment variables (K32).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
	"github.com/ardaklc20/event-driven-notification-system/internal/provider"
)

// Listen holds the HTTP listen address. The API's healthcheck subcommand reads
// only this part of the configuration (K31).
type Listen struct {
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
}

// Port returns the port part of HTTPAddr.
func (l Listen) Port() (string, error) {
	_, port, err := net.SplitHostPort(l.HTTPAddr)
	if err != nil {
		return "", fmt.Errorf("HTTP_ADDR %q: %w", l.HTTPAddr, err)
	}
	if port == "" {
		return "", fmt.Errorf("HTTP_ADDR %q: missing port", l.HTTPAddr)
	}
	return port, nil
}

// API is the configuration of cmd/api.
type API struct {
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	Listen
	LogLevel        slog.Level    `env:"LOG_LEVEL" envDefault:"info"`
	ShutdownTimeout time.Duration `env:"SHUTDOWN_TIMEOUT" envDefault:"10s"`

	// Content limits in Unicode code points, checked for new notifications
	// only (K10, K11).
	SMSContentLimit   int `env:"SMS_CONTENT_LIMIT" envDefault:"160"`
	EmailContentLimit int `env:"EMAIL_CONTENT_LIMIT" envDefault:"10000"`
	PushContentLimit  int `env:"PUSH_CONTENT_LIMIT" envDefault:"1000"`

	// BatchMaxBodyBytes is the largest POST /notifications/batch body; a larger
	// one gets 413 (K12). 16 MiB by default.
	BatchMaxBodyBytes int64 `env:"BATCH_MAX_BODY_BYTES" envDefault:"16777216"`
}

// Limits returns the content limits as the domain type.
func (c API) Limits() domain.Limits {
	return domain.Limits{SMS: c.SMSContentLimit, Email: c.EmailContentLimit, Push: c.PushContentLimit}
}

// Validate checks what the struct tags cannot.
func (c API) Validate() error {
	var errs []error
	if _, err := c.Port(); err != nil {
		errs = append(errs, err)
	}
	if c.ShutdownTimeout <= 0 {
		errs = append(errs, fmt.Errorf("SHUTDOWN_TIMEOUT must be positive, got %s", c.ShutdownTimeout))
	}
	for name, limit := range map[string]int{
		"SMS_CONTENT_LIMIT":   c.SMSContentLimit,
		"EMAIL_CONTENT_LIMIT": c.EmailContentLimit,
		"PUSH_CONTENT_LIMIT":  c.PushContentLimit,
	} {
		if limit <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %d", name, limit))
		}
	}
	if c.BatchMaxBodyBytes <= 0 {
		errs = append(errs, fmt.Errorf("BATCH_MAX_BODY_BYTES must be positive, got %d", c.BatchMaxBodyBytes))
	}
	return errors.Join(errs...)
}

// Migrate is the configuration of cmd/migrate.
type Migrate struct {
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
}

// Worker is the configuration of cmd/worker.
type Worker struct {
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
	// One URL for every channel; the channel travels in the body (A2, K14).
	ProviderURL     string           `env:"PROVIDER_URL,required,notEmpty"`
	ProviderProfile provider.Profile `env:"PROVIDER_PROFILE" envDefault:"standard"`
	HTTPTimeout     time.Duration    `env:"HTTP_TIMEOUT" envDefault:"5s"`
	// ResultBudget is the part of the lease kept for writing a result.
	ResultBudget   time.Duration `env:"RESULT_BUDGET" envDefault:"5s"`
	LeaseDuration  time.Duration `env:"LEASE_DURATION" envDefault:"30s"`
	InFlight       int           `env:"IN_FLIGHT" envDefault:"30"`
	IdleWait       time.Duration `env:"IDLE_WAIT" envDefault:"200ms"`
	ShutdownBudget time.Duration `env:"SHUTDOWN_BUDGET" envDefault:"10s"`
	LogLevel       slog.Level    `env:"LOG_LEVEL" envDefault:"info"`

	// Retries (K7): attempts in total, the first one included, and the
	// full-jitter backoff between them.
	MaxAttempts int           `env:"MAX_ATTEMPTS" envDefault:"5"`
	BackoffBase time.Duration `env:"BACKOFF_BASE" envDefault:"2s"`
	BackoffCap  time.Duration `env:"BACKOFF_CAP" envDefault:"60s"`

	// Lease recovery (K40).
	RecoveryInterval time.Duration `env:"RECOVERY_INTERVAL" envDefault:"5s"`
	RecoveryBatch    int           `env:"RECOVERY_BATCH" envDefault:"100"`

	// Channel permits (K5, K42): the least time between two permits of a
	// channel, and how long after its time a permit may start a send.
	PermitInterval time.Duration `env:"PERMIT_INTERVAL" envDefault:"11ms"`
	PermitGrace    time.Duration `env:"PERMIT_GRACE" envDefault:"100ms"`

	// Circuit breaker (K34): outage failures in a row that open it, the
	// first pause, and the cap of its doubling.
	BreakerThreshold   int           `env:"BREAKER_THRESHOLD" envDefault:"5"`
	BreakerCooldown    time.Duration `env:"BREAKER_COOLDOWN" envDefault:"30s"`
	BreakerMaxCooldown time.Duration `env:"BREAKER_MAX_COOLDOWN" envDefault:"5m"`
}

// Breaker returns the circuit breaker's settings as the domain type.
func (c Worker) Breaker() domain.BreakerPolicy {
	return domain.BreakerPolicy{
		Threshold:   c.BreakerThreshold,
		Cooldown:    c.BreakerCooldown,
		MaxCooldown: c.BreakerMaxCooldown,
	}
}

// rateWindow and rateLimit are the case's limit: 100 send starts per channel
// in any one-second window (R8).
const (
	rateWindow = time.Second
	rateLimit  = 100
)

// Validate checks what the struct tags cannot (K32).
func (c Worker) Validate() error {
	var errs []error
	if u, err := url.Parse(c.ProviderURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The URL itself is not repeated: a webhook.site URL holds its token.
		errs = append(errs, errors.New("PROVIDER_URL must be an absolute http or https URL"))
	}
	if !c.ProviderProfile.Valid() {
		errs = append(errs, fmt.Errorf("PROVIDER_PROFILE must be %s or %s, got %q",
			provider.ProfileStandard, provider.ProfileWebhookSiteFree, c.ProviderProfile))
	}
	for name, d := range map[string]time.Duration{
		"HTTP_TIMEOUT":      c.HTTPTimeout,
		"RESULT_BUDGET":     c.ResultBudget,
		"LEASE_DURATION":    c.LeaseDuration,
		"IDLE_WAIT":         c.IdleWait,
		"SHUTDOWN_BUDGET":   c.ShutdownBudget,
		"RECOVERY_INTERVAL": c.RecoveryInterval,
		"PERMIT_GRACE":      c.PermitGrace,
		"BREAKER_COOLDOWN":  c.BreakerCooldown,
	} {
		if d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %s", name, d))
		}
	}
	// The lease covers the HTTP call and writing its result (K3, K9).
	if c.LeaseDuration < c.HTTPTimeout+c.ResultBudget {
		errs = append(errs, fmt.Errorf("LEASE_DURATION (%s) must be at least HTTP_TIMEOUT + RESULT_BUDGET (%s)",
			c.LeaseDuration, c.HTTPTimeout+c.ResultBudget))
	}
	if c.InFlight < 1 {
		errs = append(errs, fmt.Errorf("IN_FLIGHT must be at least 1, got %d", c.InFlight))
	}
	if c.MaxAttempts < 1 {
		errs = append(errs, fmt.Errorf("MAX_ATTEMPTS must be at least 1, got %d", c.MaxAttempts))
	}
	if c.BackoffBase < 0 || c.BackoffCap < c.BackoffBase {
		errs = append(errs, fmt.Errorf("BACKOFF_BASE (%s) must not be negative and BACKOFF_CAP (%s) not below it",
			c.BackoffBase, c.BackoffCap))
	}
	if c.RecoveryBatch < 1 {
		errs = append(errs, fmt.Errorf("RECOVERY_BATCH must be at least 1, got %d", c.RecoveryBatch))
	}
	if c.BreakerThreshold < 1 {
		errs = append(errs, fmt.Errorf("BREAKER_THRESHOLD must be at least 1, got %d", c.BreakerThreshold))
	}
	if c.BreakerMaxCooldown < c.BreakerCooldown {
		errs = append(errs, fmt.Errorf("BREAKER_COOLDOWN (%s) must not be above BREAKER_MAX_COOLDOWN (%s)",
			c.BreakerCooldown, c.BreakerMaxCooldown))
	}
	// K42's bound: 101 starts need 101 permits, the first and the last at
	// least 100 × PERMIT_INTERVAL apart, and a start comes at most
	// PERMIT_GRACE after its permit. The first and the last start are then at
	// least 100 × PERMIT_INTERVAL − PERMIT_GRACE apart, which must not be
	// below one second.
	if time.Duration(rateLimit)*c.PermitInterval-c.PermitGrace < rateWindow {
		errs = append(errs, fmt.Errorf("100 × PERMIT_INTERVAL (%s) − PERMIT_GRACE (%s) must be at least 1s",
			c.PermitInterval, c.PermitGrace))
	}
	return errors.Join(errs...)
}

// LoadWorker parses and validates the worker configuration.
func LoadWorker(opts env.Options) (Worker, error) {
	cfg, err := env.ParseAsWithOptions[Worker](opts)
	if err != nil {
		return Worker{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Worker{}, err
	}
	return cfg, nil
}

// MockProvider is the configuration of cmd/mockprovider (K14).
type MockProvider struct {
	HTTPAddr string        `env:"HTTP_ADDR" envDefault:":8081"`
	Delay    time.Duration `env:"MOCK_DELAY" envDefault:"0s"`
	LogLevel slog.Level    `env:"LOG_LEVEL" envDefault:"info"`
}

// LoadMockProvider parses and validates the mock provider configuration.
func LoadMockProvider(opts env.Options) (MockProvider, error) {
	cfg, err := env.ParseAsWithOptions[MockProvider](opts)
	if err != nil {
		return MockProvider{}, err
	}
	if cfg.Delay < 0 {
		return MockProvider{}, fmt.Errorf("MOCK_DELAY must not be negative, got %s", cfg.Delay)
	}
	if _, err := (Listen{HTTPAddr: cfg.HTTPAddr}).Port(); err != nil {
		return MockProvider{}, err
	}
	return cfg, nil
}

// LoadAPI parses and validates the API configuration. With a zero opts it reads
// the process environment; tests pass their own variables in opts.Environment.
// Parse errors come back together in one env.AggregateError.
func LoadAPI(opts env.Options) (API, error) {
	cfg, err := env.ParseAsWithOptions[API](opts)
	if err != nil {
		return API{}, err
	}
	if err := cfg.Validate(); err != nil {
		return API{}, err
	}
	return cfg, nil
}

// LoadListen parses only the listen address.
func LoadListen(opts env.Options) (Listen, error) {
	cfg, err := env.ParseAsWithOptions[Listen](opts)
	if err != nil {
		return Listen{}, err
	}
	if _, err := cfg.Port(); err != nil {
		return Listen{}, err
	}
	return cfg, nil
}

// LoadMigrate parses the migration configuration.
func LoadMigrate(opts env.Options) (Migrate, error) {
	return env.ParseAsWithOptions[Migrate](opts)
}
