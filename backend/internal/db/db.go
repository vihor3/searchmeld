package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	// Reject removed driver options before they become PostgreSQL startup parameters.
	if _, ok := config.ConnConfig.RuntimeParams["prefer_simple_protocol"]; ok {
		return nil, fmt.Errorf("database option prefer_simple_protocol is unsupported by pgx/v5; remove it for the default extended protocol, or explicitly set default_query_exec_mode=simple_protocol")
	}
	if _, ok := config.ConnConfig.RuntimeParams["statement_cache_mode"]; ok {
		return nil, fmt.Errorf("database option statement_cache_mode is unsupported by pgx/v5; replace prepare with default_query_exec_mode=cache_statement, or describe with default_query_exec_mode=cache_describe and description_cache_capacity")
	}
	if config.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheStatement && config.ConnConfig.StatementCacheCapacity <= 0 {
		return nil, fmt.Errorf("statement_cache_capacity must be positive for default_query_exec_mode=cache_statement; use default_query_exec_mode=describe_exec for uncached extended protocol")
	}
	if config.ConnConfig.DefaultQueryExecMode == pgx.QueryExecModeCacheDescribe && config.ConnConfig.DescriptionCacheCapacity <= 0 {
		return nil, fmt.Errorf("description_cache_capacity must be positive for default_query_exec_mode=cache_describe; use default_query_exec_mode=describe_exec for uncached extended protocol")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect database: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}
