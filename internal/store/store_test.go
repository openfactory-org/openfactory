package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

func TestOpenDispatch(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cases := []struct {
		url     string
		wantSQL bool
		wantErr error
	}{
		{url: "sqlite://:memory:", wantSQL: true},
		{url: "sqlite://" + filepath.Join(dir, "a.db"), wantSQL: true},
		{url: "file:" + filepath.Join(dir, "b.db") + "?_busy_timeout=1000", wantSQL: true},
		{url: filepath.Join(dir, "c.db"), wantSQL: true},
		{url: ":memory:", wantSQL: true},
		{url: "mysql://x", wantErr: core.ErrInvalid},
		{url: "memory://", wantErr: core.ErrInvalid},
		{url: "", wantErr: core.ErrInvalid},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			s, err := store.Open(ctx, c.url)
			if c.wantErr != nil {
				if !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			_, isSQL := s.(*sqlstore.Store)
			if isSQL != c.wantSQL {
				t.Fatalf("got %T", s)
			}
			if err := s.KVSet(ctx, "k", "v"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCursorsAndPrune(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		if err := s.CreateRun(ctx, &core.Run{TeamID: "t", FactoryID: "f", CreatedAt: base.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	page1, err := s.ListRuns(ctx, core.RunFilter{TeamID: "t", ListOpts: core.ListOpts{Limit: 3}})
	if err != nil {
		t.Fatal(err)
	}
	page2, err := s.ListRuns(ctx, core.RunFilter{TeamID: "t", ListOpts: core.ListOpts{Limit: 3, Cursor: store.RunsCursor(page1)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 3 || len(page2) != 2 {
		t.Fatalf("pages = %d, %d", len(page1), len(page2))
	}
	if store.RunsCursor(nil) != "" || store.WorkItemsCursor(nil) != "" || store.DefinitionVersionsCursor(nil) != "" {
		t.Fatal("empty pages must yield empty cursors")
	}

	old := time.Now().Add(-72 * time.Hour)
	if err := s.CreateRun(ctx, &core.Run{TeamID: "t", FactoryID: "f", State: core.RunSucceeded, FinishedAt: &old}); err != nil {
		t.Fatal(err)
	}
	n, err := store.PruneOld(ctx, s, 24*time.Hour)
	if err != nil || n != 1 {
		t.Fatalf("PruneOld = %d, %v", n, err)
	}
	if _, err := store.PruneOld(ctx, s, 0); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("zero retention: %v", err)
	}
}
