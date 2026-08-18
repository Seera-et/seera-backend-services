// Package database owns the Postgres connection pool and the schema migrations
// that run against it. It knows nothing about legal documents; feature packages
// bring their own SQL.
package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgx connection pool.
type Pool struct {
	*pgxpool.Pool
}

// Connect opens the pool and verifies it, so a bad DSN or an unreachable
// database fails at boot rather than on the first request.
func Connect(ctx context.Context, dsn string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		// The DSN carries the password; never include it in the error.
		return nil, fmt.Errorf("parse database url: %w", redact(err, dsn))
	}

	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", redact(err, dsn))
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", redact(err, dsn))
	}

	return &Pool{Pool: pool}, nil
}

// Health reports whether the database is reachable right now. It is called on
// every health check, so it carries its own short timeout: a hung database
// should surface as unhealthy, not as a hung health endpoint.
func (p *Pool) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.Ping(ctx)
}

// redact strips the DSN out of an error message, in case a driver embedded it.
// The DSN carries the database password.
func redact(err error, dsn string) error {
	if dsn == "" || err == nil {
		return err
	}
	msg := err.Error()
	if cleaned := strings.ReplaceAll(msg, dsn, "[redacted dsn]"); cleaned != msg {
		return errors.New(cleaned)
	}
	return err
}
