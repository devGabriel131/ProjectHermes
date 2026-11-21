package db

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"log/slog"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

// migrations holds every migration, numbered SQL files goose runs in order. See docs/data-model.md.
//
//go:embed migrations/*.sql
var migrations embed.FS

// baselineSQL is version 1. A Go migration runs it instead of goose's SQL runner, so that a database
// AutoMigrate built adopts it without DDL.
//
//go:embed migrations/00001_baseline.sql
var baselineSQL string

// baselineTables are the tables baselineSQL creates.
var baselineTables = []string{
	"billing_checkout_attempts", "billing_contracts", "billing_obligations", "billing_payments",
	"billing_suspensions", "billing_webhook_events", "enrollments", "links", "sessions", "settings",
	"students", "users",
}

// ErrPartialSchema means the schema holds some baseline tables but not all, so neither creating the
// baseline nor adopting it is safe.
var ErrPartialSchema = errors.New("schema holds only part of the baseline")

// Migrate applies each pending migration to the connection's current schema under a session advisory lock
// on that schema (goose creates goose_db_version before taking it), and logs database_migrated for each
// version applied. A schema with all the baseline tables adopts version 1 with no DDL; one with none gets
// the baseline; a partial one fails with ErrPartialSchema before goose creates its version table.
func (db *DB) Migrate(ctx context.Context, log *slog.Logger) error {
	var schema string
	var versioned bool
	if err := db.sql.QueryRowContext(ctx,
		`SELECT current_schema(), to_regclass(format('%I.goose_db_version', current_schema())) IS NOT NULL`,
	).Scan(&schema, &versioned); err != nil {
		return err
	}
	// goose creates goose_db_version before it runs anything, so a partial schema is refused first. Version
	// 1 decides again inside its transaction, under the lock.
	if !versioned {
		if _, err := baselinePresent(ctx, db.sql); err != nil {
			return err
		}
	}
	var adopted bool
	baseline := goose.NewGoMigration(1, &goose.GoFunc{RunTx: func(ctx context.Context, tx *sql.Tx) error {
		var err error
		if adopted, err = baselinePresent(ctx, tx); err != nil || adopted {
			return err
		}
		_, err = tx.ExecContext(ctx, baselineSQL)
		return err
	}}, nil)
	// One lock per schema, as goose_db_version is per schema: parallel tests each migrate their own. It is
	// retried every second; ctx bounds the wait.
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(int64(crc32.ChecksumIEEE([]byte(schema)))),
		lock.WithLockTimeout(1, 60),
	)
	if err != nil {
		return err
	}
	dir, err := fs.Sub(migrations, "migrations")
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db.sql, dir,
		goose.WithExcludeNames([]string{"00001_baseline.sql"}),
		goose.WithGoMigrations(baseline),
		goose.WithSessionLocker(locker),
	)
	if err != nil {
		return err
	}
	results, err := provider.Up(ctx)
	if partial, ok := errors.AsType[*goose.PartialError](err); ok {
		results = partial.Applied
	}
	// The logger's own version field is the build, so the migration's number is migration.
	for _, r := range results {
		log.Info("database_migrated", "migration", r.Source.Version, "adopted", r.Source.Version == 1 && adopted,
			"duration_ms", r.Duration.Milliseconds())
	}
	return err
}

// baselinePresent reports whether the current schema holds every baseline table (true) or none (false); a
// partial schema is ErrPartialSchema.
func baselinePresent(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (bool, error) {
	var missing int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM unnest($1::text[]) AS name
		WHERE to_regclass(format('%I.%I', current_schema(), name)) IS NULL`, baselineTables).Scan(&missing); err != nil {
		return false, err
	}
	switch missing {
	case 0:
		return true, nil
	case len(baselineTables):
		return false, nil
	}
	return false, fmt.Errorf("%w: %d of its %d tables are missing", ErrPartialSchema, missing, len(baselineTables))
}
