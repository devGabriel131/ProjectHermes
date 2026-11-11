//go:build integration

// Package testdb gives each integration test its own PostgreSQL schema on TEST_DATABASE_URL, migrated at open and dropped with the test. See docs/testing.md.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open isolates each test in a schema, including queries through db.DB().
func Open(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") {
		t.Fatal("TEST_DATABASE_URL must be a PostgreSQL URL for a dedicated test database")
	}
	var random [8]byte
	rand.Read(random[:])
	schema := "pwtest_" + hex.EncodeToString(random[:])
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{
		DisableAutomaticPing:   true,
		SkipDefaultTransaction: true,
		Logger:                 logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	pool.SetMaxOpenConns(10)
	pool.SetMaxIdleConns(10)
	// Schema identifiers contain only this fixed prefix and internally generated hex.
	if err := db.WithContext(ctx).Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := db.WithContext(cleanup).Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	if len(models) > 0 {
		if err := db.WithContext(ctx).AutoMigrate(models...); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// AwaitBlocked returns once a session on db waits for a lock held by blocker's open
// transaction, so tests release the lock on database state instead of elapsed time.
// It fails the test if results yields first (the call finished without waiting) or ctx ends.
func AwaitBlocked[T any](t testing.TB, ctx context.Context, db, blocker *gorm.DB, results <-chan T) {
	t.Helper()
	var blockerPID int
	if err := blocker.Raw("SELECT pg_backend_pid()").Scan(&blockerPID).Error; err != nil {
		t.Fatal(err)
	}
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := db.WithContext(ctx).Raw(
			"SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE ? = ANY(pg_blocking_pids(pid)))",
			blockerPID,
		).Scan(&waiting).Error; err != nil {
			t.Fatalf("observe lock wait: %v", err)
		}
		if waiting {
			return
		}
		select {
		case got := <-results:
			t.Fatalf("returned before blocking: %+v", got)
		case <-ctx.Done():
			t.Fatalf("never blocked: %v", ctx.Err())
		case <-poll.C:
		}
	}
}
