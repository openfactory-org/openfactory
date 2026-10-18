// Package store opens core.Store implementations by URL and provides helpers
// shared by their callers (pagination cursors, retention).
//
// Supported URLs:
//
//	sqlite:///var/lib/openfactory/of.db   sqlite://relative/path.db   file:of.db?_busy_timeout=5000
//	/plain/path/of.db                      postgres://user:pass@host/db  postgresql://...
//	sqlite://:memory:   :memory:   (a SQLite database in memory)
package store

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/postgres"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// Pagination limits applied to ListOpts.
const (
	DefaultLimit = storeutil.DefaultLimit
	MaxLimit     = storeutil.MaxLimit
)

// Pruner is implemented by stores that support retention cleanup (all
// built-in stores do).
type Pruner = storeutil.Pruner

// Open opens a store, dispatching on the URL scheme.
func Open(ctx context.Context, url string) (core.Store, error) {
	u := strings.TrimSpace(url)
	lower := strings.ToLower(u)
	switch {
	case u == "":
		return nil, fmt.Errorf("%w: empty store url", core.ErrInvalid)
	case strings.HasPrefix(lower, "postgres://"), strings.HasPrefix(lower, "postgresql://"):
		return postgres.Open(ctx, u)
	case strings.HasPrefix(lower, "sqlite://"), strings.HasPrefix(lower, "file:"):
		return sqlite.Open(ctx, u)
	case u == ":memory:":
		return sqlite.Open(ctx, u)
	case strings.Contains(u, "://"):
		return nil, fmt.Errorf("%w: unsupported store url scheme in %q", core.ErrInvalid, schemeOf(u))
	default:
		return sqlite.Open(ctx, u)
	}
}

func schemeOf(u string) string {
	s, _, _ := strings.Cut(u, "://")
	return s
}

// NextCursor returns the opaque cursor continuing a (created_at DESC, id DESC)
// listing after the given item. Use it with the last element of a page from
// ListRuns, ListWorkItems or ListDefinitionVersions.
func NextCursor(createdAt time.Time, id string) string {
	return storeutil.EncodeCursor(createdAt, id)
}

// RunsCursor returns the cursor for the page after runs ("" when runs is empty).
func RunsCursor(runs []core.Run) string {
	if len(runs) == 0 {
		return ""
	}
	r := runs[len(runs)-1]
	return NextCursor(r.CreatedAt, r.ID)
}

// WorkItemsCursor returns the cursor for the page after items ("" when empty).
func WorkItemsCursor(items []core.WorkItem) string {
	if len(items) == 0 {
		return ""
	}
	w := items[len(items)-1]
	return NextCursor(w.CreatedAt, w.ID)
}

// DefinitionVersionsCursor returns the cursor for the page after versions.
func DefinitionVersionsCursor(versions []core.DefinitionVersion) string {
	if len(versions) == 0 {
		return ""
	}
	v := versions[len(versions)-1]
	return NextCursor(v.CreatedAt, v.ID)
}

// PruneOld deletes data older than olderThan: terminal runs (with their
// messages) finished before the cutoff, resolved inbox items and webhook
// deliveries. It returns the number of deleted rows, or core.ErrInvalid if
// the store does not support retention.
func PruneOld(ctx context.Context, s core.Store, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		return 0, fmt.Errorf("%w: retention must be positive", core.ErrInvalid)
	}
	p, ok := s.(Pruner)
	if !ok {
		return 0, fmt.Errorf("%w: store does not support retention", core.ErrInvalid)
	}
	return p.PruneBefore(ctx, time.Now().Add(-olderThan))
}
