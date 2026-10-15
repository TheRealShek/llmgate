package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolLimits bounds database connections and pool startup time.
type PoolLimits struct {
	MaxConns       int32
	ConnectTimeout time.Duration
}

// OpenPool connects and pings within ConnectTimeout. The caller owns and closes the pool.
// Connection diagnostics stay private because they can contain database credentials.
func OpenPool(ctx context.Context, databaseURL string, limits PoolLimits) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database configuration: %w", &databaseError{category: ErrInvalidDatabaseConfig, cause: err})
	}

	if limits.MaxConns <= 0 || limits.ConnectTimeout <= 0 {
		return nil, fmt.Errorf("validate database limits: %w", ErrInvalidDatabaseConfig)
	}

	cfg.MaxConns = limits.MaxConns
	cfg.MinConns = 0
	cfg.ConnConfig.ConnectTimeout = limits.ConnectTimeout

	connectCtx, cancel := context.WithTimeout(ctx, limits.ConnectTimeout)
	defer cancel()

	pool, err := pgxpool.NewWithConfig(connectCtx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open database pool: %w", &databaseError{category: ErrDatabaseUnavailable, cause: err})
	}

	if err := pool.Ping(connectCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect database: %w", &databaseError{category: ErrDatabaseUnavailable, cause: err})
	}

	return pool, nil
}
