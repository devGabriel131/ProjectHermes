// Package db opens the one PostgreSQL pool every query runs on, whichever library issues it, and migrates
// the schema with goose. See docs/architecture.md, docs/data-model.md and docs/operations/startup-shutdown.md.
package db

import (
	"context"
	"database/sql"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// maxConns bounds the pool, shared by GORM and the session store.
const maxConns = 10

// DB is the pgx pool and the handles onto it. GORM borrows a pool connection per call through database/sql,
// which keeps none idle, so GORM and the scs session store wait in the pool's queue and pass its tracer.
type DB struct {
	Pool *pgxpool.Pool
	GORM *gorm.DB
	sql  *sql.DB
}

// Open configures the pool for dsn with tracer timing every query. It connects nothing: the first query,
// or Pool.Ping, opens a connection.
func Open(dsn string, tracer pgx.QueryTracer) (*DB, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	config.MaxConns = maxConns
	// dblog's pool-wait count needs every connection opened by an acquire, never by the health check.
	config.MinConns, config.MinIdleConns = 0, 0
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		return nil, err
	}
	sqlDB := stdlib.OpenDBFromPool(pool)
	// GORM's own logger is silent: the tracer is the only thing that sees queries, and it never logs SQL.
	orm, err := gorm.Open(postgres.New(postgres.Config{Conn: sqlDB}), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	})
	if err != nil {
		db := &DB{Pool: pool, sql: sqlDB}
		return nil, errors.Join(err, db.Close())
	}
	return &DB{Pool: pool, GORM: orm, sql: sqlDB}, nil
}

// Close closes database/sql's handle, then the pool, which waits for every borrowed connection to come back.
func (db *DB) Close() error {
	err := db.sql.Close()
	db.Pool.Close()
	return err
}
