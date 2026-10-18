// Package postgres provides the PostgreSQL core.Store implementation backed by
// a pgx/v5 connection pool. Queries are shared with the SQLite implementation
// through internal/store/sqlstore; queued runs are claimed with
// SELECT ... FOR UPDATE SKIP LOCKED so many workers can poll concurrently.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the embedded PostgreSQL migrations.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// migrationLockKey is an arbitrary constant identifying the schema migration
// advisory lock ("openfact" in ASCII).
const migrationLockKey = 0x6f70656e66616374

// Dialect is the PostgreSQL SQL dialect.
var Dialect = sqlstore.Dialect{
	Name:                  "postgres",
	NumberedPlaceholders:  true,
	IsUniqueViolation:     func(err error) bool { return pgCode(err) == "23505" },
	IsForeignKeyViolation: func(err error) bool { return pgCode(err) == "23503" },
	ILike:                 "ILIKE",
	SanitizeJSON:          sqlstore.StripJSONNul,
	SkipLocked:            " FOR UPDATE SKIP LOCKED",
	OutboxSeq:             "seq",
	TxSavepoints:          true,
	MigrationLockSQL:      fmt.Sprintf("SELECT pg_advisory_xact_lock(%d)", int64(migrationLockKey)),
}

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// Open connects to PostgreSQL (postgres:// or postgresql:// URL, or a
// key=value connection string) and applies pending migrations.
func Open(ctx context.Context, dsn string) (core.Store, error) {
	return OpenStore(ctx, dsn)
}

// OpenStore is Open returning the concrete type.
func OpenStore(ctx context.Context, dsn string) (*sqlstore.Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("%w: postgres dsn: %w", core.ErrInvalid, err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	db := stdlib.OpenDBFromPool(pool)
	n := newNotifier(db, pool)
	s := sqlstore.New(db, Dialect, func() error { n.close(); pool.Close(); return nil })
	s.SetNotifier(n)
	if err := s.Migrate(ctx, Migrations()); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}
