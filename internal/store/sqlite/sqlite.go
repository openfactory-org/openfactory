// Package sqlite provides the SQLite core.Store implementation (pure Go, via
// modernc.org/sqlite). The database runs in WAL mode with foreign keys
// enabled, a busy timeout, and IMMEDIATE transactions so that concurrent
// writers queue instead of failing.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migrations returns the embedded SQLite migrations.
func Migrations() fs.FS {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Dialect is the SQLite SQL dialect.
var Dialect = sqlstore.Dialect{
	Name:                  "sqlite",
	IsUniqueViolation:     isUnique,
	IsForeignKeyViolation: isForeignKey,
	ILike:                 "LIKE", // ASCII case-insensitive by default
	OutboxSeq:             "rowid",
}

func code(err error) int {
	var se *sqlite.Error
	if errors.As(err, &se) {
		return se.Code()
	}
	return 0
}

func isUnique(err error) bool {
	c := code(err)
	return c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}

func isForeignKey(err error) bool { return code(err) == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY }

// Open opens (creating if needed) the SQLite database described by dsn and
// applies pending migrations. Accepted forms: a file path, "file:<path>[?query]",
// "sqlite://<path>" and ":memory:". Driver parameters already present in a
// "file:" DSN are kept; WAL, foreign keys, busy timeout and immediate
// transactions are added when absent.
func Open(ctx context.Context, dsn string) (core.Store, error) {
	return OpenStore(ctx, dsn)
}

// OpenStore is Open returning the concrete type.
func OpenStore(ctx context.Context, dsn string) (*sqlstore.Store, error) {
	full, memory, err := buildDSN(dsn)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", full)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open: %w", err)
	}
	if memory {
		// Every connection to :memory: is a separate database.
		db.SetMaxOpenConns(1)
	} else {
		db.SetMaxOpenConns(8)
	}
	db.SetMaxIdleConns(4)
	s := sqlstore.New(db, Dialect, nil)
	if err := ping(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: connect: %w", err)
	}
	if err := s.Migrate(ctx, Migrations()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// pingBusyWait bounds how long ping retries a connection that fails with
// SQLITE_BUSY.
const pingBusyWait = 10 * time.Second

// ping opens the first connection. When several processes open a new file
// at the same time, the switch to WAL can fail with SQLITE_BUSY without
// waiting for the busy timeout, so ping retries that error for a bounded time.
func ping(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(pingBusyWait)
	for {
		err := db.PingContext(ctx)
		if err == nil || code(err)&0xff != sqlite3.SQLITE_BUSY || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func buildDSN(dsn string) (string, bool, error) {
	dsn = strings.TrimSpace(dsn)
	dsn = strings.TrimPrefix(dsn, "sqlite://")
	if dsn == "" {
		return "", false, fmt.Errorf("%w: empty sqlite dsn", core.ErrInvalid)
	}
	path, rawQuery := dsn, ""
	if strings.HasPrefix(dsn, "file:") {
		path = strings.TrimPrefix(dsn, "file:")
		path, rawQuery, _ = strings.Cut(path, "?")
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "", false, fmt.Errorf("%w: sqlite dsn query: %w", core.ErrInvalid, err)
	}
	memory := path == ":memory:" || q.Get("mode") == "memory"
	if !memory {
		if dir := filepath.Dir(path); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return "", false, fmt.Errorf("sqlite: create directory: %w", err)
			}
		}
	}
	setDefault := func(key, val string, aliases ...string) {
		for _, k := range append([]string{key}, aliases...) {
			if q.Has(k) {
				return
			}
		}
		q.Set(key, val)
	}
	setDefault("_busy_timeout", "10000", "_timeout")
	setDefault("_foreign_keys", "1", "_fk")
	setDefault("_txlock", "immediate")
	if !memory {
		setDefault("_journal_mode", "WAL", "_journal")
		setDefault("_synchronous", "NORMAL", "_sync")
	}
	return "file:" + path + "?" + q.Encode(), memory, nil
}
