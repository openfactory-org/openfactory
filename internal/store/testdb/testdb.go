// Package testdb opens fresh, migrated SQLite in-memory stores for tests.
package testdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/store/sqlite"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
)

// memoryDSN is the DSN that sqlite.OpenStore builds for ":memory:". New checks
// that foreign keys are on, so a change of the defaults shows up in tests.
const memoryDSN = "file::memory:?_busy_timeout=10000&_foreign_keys=1&_txlock=immediate"

var (
	imageOnce sync.Once
	image     []byte
	imageErr  error
)

// migratedImage returns the bytes of a migrated in-memory database. The
// migrations run once per test binary.
func migratedImage() ([]byte, error) {
	imageOnce.Do(func() {
		ctx := context.Background()
		s, err := sqlite.OpenStore(ctx, ":memory:")
		if err != nil {
			imageErr = fmt.Errorf("open sqlite :memory: store: %w", err)
			return
		}
		defer func() { _ = s.Close() }()
		image, imageErr = withConn(ctx, s.DB(), func(c serializer) ([]byte, error) { return c.Serialize() })
	})
	return image, imageErr
}

// serializer is the part of the SQLite driver connection that New uses.
type serializer interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

func withConn(ctx context.Context, db *sql.DB, f func(serializer) ([]byte, error)) ([]byte, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	var out []byte
	err = conn.Raw(func(dc any) error {
		c, ok := dc.(serializer)
		if !ok {
			return errors.New("the sqlite driver connection cannot serialize")
		}
		var ferr error
		out, ferr = f(c)
		return ferr
	})
	return out, err
}

// New returns a new migrated SQLite ":memory:" store. Each call gets its own
// database, a copy of a database that was migrated once per test binary, so
// tests stay isolated. The store keeps one connection, as sqlite.OpenStore
// does for an in-memory store. The store is closed when the test ends.
func New(t testing.TB) *sqlstore.Store {
	t.Helper()
	ctx := context.Background()
	img, err := migratedImage()
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	db, err := sql.Open("sqlite", memoryDSN)
	if err != nil {
		t.Fatalf("testdb: open sqlite: %v", err)
	}
	// Every connection to :memory: is a separate database: keep the one that
	// holds the copy.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := withConn(ctx, db, func(c serializer) ([]byte, error) { return nil, c.Deserialize(img) }); err != nil {
		_ = db.Close()
		t.Fatalf("testdb: load the migrated database: %v", err)
	}
	var fk int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		_ = db.Close()
		t.Fatalf("testdb: foreign keys are not on (%d, %v)", fk, err)
	}
	s := sqlstore.New(db, sqlite.Dialect, nil)
	t.Cleanup(func() { _ = s.Close() })
	return s
}
