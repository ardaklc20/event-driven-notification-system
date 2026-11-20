// Package postgres holds the connection pool and, from slice 2 on, all SQL (K17).
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PingTimeout bounds the readiness ping (K30). It is shorter than the
// healthcheck subcommand's 2-second limit, so /readyz answers 503 before the
// healthcheck gives up.
const PingTimeout = 1 * time.Second

// NewPool opens a pgxpool for databaseURL. It does not connect yet; the first
// query or ping does.
func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	return pool, nil
}

// Ping checks that the database answers within PingTimeout.
func Ping(ctx context.Context, pool *pgxpool.Pool) error {
	ctx, cancel := context.WithTimeout(ctx, PingTimeout)
	defer cancel()
	return pool.Ping(ctx)
}
