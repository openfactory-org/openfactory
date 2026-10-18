package sqlstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
)

// Migration is one versioned schema change.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// LoadMigrations reads files named "<version>_<name>.sql" from fsys (root
// directory) and returns them sorted by version.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		num, name, _ := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: file name must start with a positive version number", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Migrate applies every migration in fsys that is not yet recorded in the
// schema_migrations table. Each migration runs in its own transaction.
func (s *Store) Migrate(ctx context.Context, fsys fs.FS) error {
	migs, err := LoadMigrations(fsys)
	if err != nil {
		return err
	}
	if err := s.createMigrationsTable(ctx); err != nil {
		return fmt.Errorf("%s: create schema_migrations: %w", s.d.Name, err)
	}
	for _, m := range migs {
		applied, err := s.applyMigration(ctx, m)
		if err != nil {
			return fmt.Errorf("%s: migration %d_%s: %w", s.d.Name, m.Version, m.Name, err)
		}
		if applied {
			slog.Debug("store: applied migration", "dialect", s.d.Name, "version", m.Version, "name", m.Name)
		}
	}
	return nil
}

// beginMigrationTx starts a transaction that holds the dialect's migration
// lock, so concurrent openers (replicas that start together) run one at a time.
func (s *Store) beginMigrationTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	if s.d.MigrationLockSQL != "" {
		if _, err := tx.ExecContext(ctx, s.d.MigrationLockSQL); err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("acquire migration lock: %w", err)
		}
	}
	return tx, nil
}

// createMigrationsTable creates schema_migrations under the migration lock.
// Without the lock, two concurrent CREATE TABLE IF NOT EXISTS on PostgreSQL
// can both pass the existence check, and one then fails on a duplicate type.
func (s *Store) createMigrationsTable(ctx context.Context) error {
	tx, err := s.beginMigrationTx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_ns BIGINT NOT NULL
	)`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) applyMigration(ctx context.Context, m Migration) (bool, error) {
	tx, err := s.beginMigrationTx(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var name string
	err = tx.QueryRowContext(ctx, s.rebind("SELECT name FROM schema_migrations WHERE version=?"), m.Version).Scan(&name)
	switch {
	case err == nil && name == m.Name:
		return false, nil
	case err == nil:
		return false, fmt.Errorf("the database recorded migration %d as %q, not %q: it was made by a different schema; make the database again", m.Version, name, m.Name)
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, s.rebind("INSERT INTO schema_migrations (version, name, applied_ns) VALUES (?,?,?)"),
		m.Version, m.Name, now().UnixNano()); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
