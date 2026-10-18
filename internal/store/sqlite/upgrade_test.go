package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/storetest"
)

// TestUpgradeFromEarlierReleases creates a database file at each earlier
// release's schema, seeds it, then opens it with the current code (which
// applies the pending migrations) and checks the data survived.
func TestUpgradeFromEarlierReleases(t *testing.T) {
	ctx := context.Background()
	for i := 0; i < storetest.Releases(); i++ {
		t.Run(storetest.ReleaseName(i), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "openfactory.db")
			full, _, err := buildDSN(path)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", full)
			if err != nil {
				t.Fatal(err)
			}
			old := sqlstore.New(db, Dialect, nil)
			storetest.MigrateTo(t, old, storetest.ReleaseMigrations(t, "sqlite", Migrations(), i))
			fx := storetest.SeedForUpgrade(t, old)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}

			s, err := OpenStore(ctx, path)
			if err != nil {
				t.Fatalf("open with current migrations: %v", err)
			}
			defer s.Close()
			migs, err := sqlstore.LoadMigrations(Migrations())
			if err != nil {
				t.Fatal(err)
			}
			var n, top int
			if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*), MAX(version) FROM schema_migrations").Scan(&n, &top); err != nil {
				t.Fatal(err)
			}
			if n != len(migs) || top != migs[len(migs)-1].Version {
				t.Fatalf("schema_migrations: %d rows, max %d; want %d, %d", n, top, len(migs), migs[len(migs)-1].Version)
			}
			storetest.VerifyUpgrade(t, s, fx)
		})
	}
}

// TestUpgradeRefusesPreSquashDatabase checks that a database made by the
// migration history before the single baseline is refused, not changed.
func TestUpgradeRefusesPreSquashDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openfactory.db")
	full, _, err := buildDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", full)
	if err != nil {
		t.Fatal(err)
	}
	storetest.MakePreSquashDatabase(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(context.Background(), path)
	if err == nil {
		_ = s.Close()
	}
	storetest.RequireOldDatabaseRefused(t, err)
}
