// Package config reads each program's settings from environment variables (K32).
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/caarlos0/env/v11"
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
