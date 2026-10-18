package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// maxScanPages bounds the store pages that one filtered list request reads.
const maxScanPages = 20

// runExtraFilter holds the run filters that the store does not index:
// source and model. The API applies them to store
// pages. The creator filter is core.RunFilter.CreatorID.
type runExtraFilter struct {
	sources map[core.RunSource]bool
	models  []string
}

// creatorParam reads the creator filter: a principal ID, or "me" for the
// caller.
func creatorParam(v string, p *Principal) (string, error) {
	v = strings.TrimSpace(v)
	if v != "me" {
		return v, nil
	}
	if id := p.UserID(); id != "" {
		return id, nil
	}
	return "", errInvalid("creator=me needs a user or a service account as the caller")
}

func runExtraFilterFrom(r *http.Request) (runExtraFilter, error) {
	var f runExtraFilter
	for _, v := range qList(r, "source") {
		src, ok := core.RunSourceName(v)
		if !ok {
			names := make([]string, len(core.RunSources))
			for i, s := range core.RunSources {
				names[i] = string(s)
			}
			return f, errInvalid("unknown source %q (expected one of %s)", v, strings.Join(names, ", "))
		}
		if f.sources == nil {
			f.sources = map[core.RunSource]bool{}
		}
		f.sources[src] = true
	}
	f.models = qList(r, "model")
	return f, nil
}

func (f runExtraFilter) empty() bool {
	return len(f.sources) == 0 && len(f.models) == 0
}

func (f runExtraFilter) keep(r *core.Run) bool {
	if len(f.sources) > 0 {
		src, _ := r.SourceAndRef()
		if !f.sources[src] {
			return false
		}
	}
	if len(f.models) > 0 {
		ok := false
		for _, m := range f.models {
			ok = ok || strings.EqualFold(m, r.Config.Harness.Model)
		}
		if !ok {
			return false
		}
	}
	return true
}

// scanFiltered reads store pages in the order of pg and keeps the items
// that pass keep, until it has one page. It reads at most maxScanPages
// store pages. When it stops early, the next cursor points after the last
// item that it read, so the client continues from there.
func scanFiltered[T any](pg pageParams, fetch func(core.ListOpts) ([]T, error), keep func(T) bool, key func(T) (timeT, string)) ([]T, string, error) {
	batch := max(pg.Limit*2, 100)
	cursor := pg.Cursor
	var out []T
	for i := 0; i < maxScanPages; i++ {
		page, err := fetch(core.ListOpts{Limit: batch, Cursor: cursor, Ascending: pg.Asc})
		if err != nil {
			return nil, "", err
		}
		for _, it := range page {
			if !keep(it) {
				continue
			}
			out = append(out, it)
			if len(out) > pg.Limit {
				out = out[:pg.Limit]
				t, id := key(out[pg.Limit-1])
				return out, encodeCursor(t, id), nil
			}
		}
		if len(page) < batch {
			return out, "", nil
		}
		t, id := key(page[len(page)-1])
		cursor = encodeCursor(t, id)
	}
	return out, cursor, nil
}

type requestSourceKey struct{}

// withRequestSource records the tool of a request that starts a run: "cli"
// for the openfactory command line (its User-Agent), "api" for another API
// key client, and "dashboard" for a browser session. It is the stored
// Run.TriggeredBy value, and the API shows it as source.
func withRequestSource(ctx context.Context, r *http.Request, p *Principal) context.Context {
	src := "api"
	switch {
	case strings.HasPrefix(r.UserAgent(), "openfactory/"):
		src = "cli"
	case p != nil && p.APIKey == nil && p.User != nil:
		src = "dashboard"
	}
	return context.WithValue(ctx, requestSourceKey{}, src)
}

// requestSource returns the value that withRequestSource recorded, or "api".
func requestSource(ctx context.Context) string {
	if v, ok := ctx.Value(requestSourceKey{}).(string); ok {
		return v
	}
	return "api"
}
