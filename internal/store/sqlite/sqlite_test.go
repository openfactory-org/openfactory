package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/storetest"
)

func TestConformanceFile(t *testing.T) {
	storetest.Run(t, func(t *testing.T) core.Store {
		s, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "of.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestConformanceMemory(t *testing.T) {
	storetest.Run(t, func(t *testing.T) core.Store {
		s, err := sqlite.Open(context.Background(), ":memory:")
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestReopenKeepsDataAndSkipsAppliedMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "nested", "dir", "of.db")
	s, err := sqlite.Open(ctx, "sqlite://"+path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.KVSet(ctx, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := sqlite.OpenStore(ctx, "file:"+path+"?_busy_timeout=2000")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v, err := s2.KVGet(ctx, "k")
	if err != nil || v != "v" {
		t.Fatalf("KVGet = %q, %v", v, err)
	}
	var n int
	if err := s2.DB().QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&n); err != nil {
		t.Fatal(err)
	}
	migs, err := sqlstore.LoadMigrations(sqlite.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if n != len(migs) {
		t.Fatalf("schema_migrations rows = %d, want %d", n, len(migs))
	}
	var mode string
	if err := s2.DB().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, %v", mode, err)
	}
	var fk int
	if err := s2.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil || fk != 1 {
		t.Fatalf("foreign_keys = %d, %v", fk, err)
	}
}

func TestBadDSN(t *testing.T) {
	if _, err := sqlite.Open(context.Background(), ""); err == nil {
		t.Fatal("expected error for empty dsn")
	}
	dsn := "file:" + filepath.Join(t.TempDir(), "x.db") + "?_txlock=bogus"
	if _, err := sqlite.Open(context.Background(), dsn); err == nil {
		t.Fatal("expected error for invalid driver parameter")
	}
}

func TestOpenRefusesADatabaseWithOtherMigrations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "of.db")
	s, err := sqlite.OpenStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec("UPDATE schema_migrations SET name = 'init' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s2, err := sqlite.OpenStore(ctx, path); err == nil {
		s2.Close()
		t.Fatal("open succeeded on a database with a different migration 1")
	}
}
