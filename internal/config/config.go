// Package config reads each program's settings from environment variables (K32).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/ardaklc20/event-driven-notification-system/internal/domain"
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
	return errors.Join(errs...)
}

// Migrate is the configuration of cmd/migrate.
type Migrate struct {
	DatabaseURL string `env:"DATABASE_URL,required,notEmpty"`
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
