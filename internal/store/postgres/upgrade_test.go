package postgres

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/storetest"
)

// withSearchPath returns dsn with its search_path set to schema.
func withSearchPath(dsn, schema string) string {
	if u, err := url.Parse(dsn); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
		q := u.Query()
		q.Set("search_path", schema)
		u.RawQuery = q.Encode()
		return u.String()
	}
	return dsn + " search_path=" + schema
}

// TestUpgradeFromEarlierReleases creates a schema at each earlier release's
// migrations in a fresh PostgreSQL schema, seeds it, then opens it with the
// current code and checks the data survived. Set OF_TEST_POSTGRES_DSN to run.
func TestUpgradeFromEarlierReleases(t *testing.T) {
	dsn := os.Getenv("OF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OF_TEST_POSTGRES_DSN not set")
	}
	ctx := context.Background()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for i := 0; i < storetest.Releases(); i++ {
		t.Run(storetest.ReleaseName(i), func(t *testing.T) {
			schema := "of_upgrade_" + strings.ToLower(strings.TrimPrefix(core.NewID("s"), "s_"))
			if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
			sdsn := withSearchPath(dsn, schema)

			cfg, err := pgxpool.ParseConfig(sdsn)
			if err != nil {
				t.Fatal(err)
			}
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			old := sqlstore.New(stdlib.OpenDBFromPool(pool), Dialect, func() error { pool.Close(); return nil })
			storetest.MigrateTo(t, old, storetest.ReleaseMigrations(t, "postgres", Migrations(), i))
			fx := storetest.SeedForUpgrade(t, old)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}

			s, err := OpenStore(ctx, sdsn)
			if err != nil {
				t.Fatalf("open with current migrations: %v", err)
			}
			defer s.Close()
			storetest.VerifyUpgrade(t, s, fx)
		})
	}
}

// newTestSchema creates a fresh PostgreSQL schema, dropped when the test
// ends, and returns a DSN that uses it.
func newTestSchema(t *testing.T, admin *sql.DB, dsn, prefix string) string {
	t.Helper()
	schema := prefix + strings.ToLower(strings.TrimPrefix(core.NewID("s"), "s_"))
	if _, err := admin.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.ExecContext(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	return withSearchPath(dsn, schema)
}

// TestUpgradeRefusesPreSquashDatabase checks that a database made by the
// migration history before the single baseline is refused, not changed.
func TestUpgradeRefusesPreSquashDatabase(t *testing.T) {
	dsn := os.Getenv("OF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OF_TEST_POSTGRES_DSN not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	sdsn := newTestSchema(t, admin, dsn, "of_presquash_")
	db, err := sql.Open("pgx", sdsn)
	if err != nil {
		t.Fatal(err)
	}
	storetest.MakePreSquashDatabase(t, db)
	_ = db.Close()
	s, err := OpenStore(context.Background(), sdsn)
	if err == nil {
		_ = s.Close()
	}
	storetest.RequireOldDatabaseRefused(t, err)
}

// TestConcurrentFirstOpen opens an empty schema from several goroutines at
// once, as replicas do on a first install; every open must succeed.
func TestConcurrentFirstOpen(t *testing.T) {
	dsn := os.Getenv("OF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OF_TEST_POSTGRES_DSN not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	for round := 0; round < 5; round++ {
		sdsn := newTestSchema(t, admin, dsn, "of_race_")
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s, err := OpenStore(context.Background(), sdsn)
				if err != nil {
					t.Errorf("round %d: concurrent first open: %v", round, err)
					return
				}
				_ = s.Close()
			}()
		}
		wg.Wait()
	}
}
