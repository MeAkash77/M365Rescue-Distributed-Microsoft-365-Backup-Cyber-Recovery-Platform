// Package db owns the PostgreSQL connection pool and schema migration.
package db

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schema string

// Connect retries for up to ~60s so containers can start in any order.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 30; i++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("postgres unavailable: %w", lastErr)
}

// Migrate is idempotent and serialised across replicas with an advisory lock.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(727365)"); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), "SELECT pg_advisory_unlock(727365)")
	_, err = conn.Exec(ctx, schema)
	return err
}
