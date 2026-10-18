package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/postgres"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/storetest"
)

// Set OF_TEST_POSTGRES_DSN (e.g. postgres://postgres:postgres@localhost:5432/of_test?sslmode=disable)
// to run the conformance suite against a real database. The suite uses unique
// IDs, so a shared database is fine.
func TestConformance(t *testing.T) {
	dsn := os.Getenv("OF_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("OF_TEST_POSTGRES_DSN not set")
	}
	storetest.Run(t, func(t *testing.T) core.Store {
		s, err := postgres.Open(context.Background(), dsn)
		if err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestMigrationsLoad(t *testing.T) {
	migs, err := sqlstore.LoadMigrations(postgres.Migrations())
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 || migs[0].Version != 1 {
		t.Fatalf("unexpected migrations: %+v", migs)
	}
}

func TestBadDSN(t *testing.T) {
	if _, err := postgres.Open(context.Background(), "postgres://%zz"); err == nil {
		t.Fatal("expected error")
	}
}
