package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

const (
	defaultPageSize = 50
	maxPageSize     = 200
	maxBodyBytes    = 8 << 20 // definitions can be large
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// decodeJSON decodes the request body into v. An empty body is allowed when
// optional is true.
func decodeJSON(r *http.Request, v any, optional bool) error {
	body := http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	data, err := io.ReadAll(body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return errInvalid("request body exceeds %d bytes", maxBodyBytes)
		}
		return errInvalid("reading body: %v", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		if optional {
			return nil
		}
		return errInvalid("request body is required")
	}
	if err := json.Unmarshal(data, v); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field != "" {
			return errInvalid("field %q must be of type %s", ute.Field, ute.Type.String())
		}
		return errInvalid("malformed JSON body: %v", err)
	}
	return nil
}

// ---------- query helpers ----------

func qBool(r *http.Request, name string) (bool, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, errInvalid("query parameter %q must be a boolean", name)
	}
	return b, nil
}

func qInt(r *http.Request, name string, def int) (int, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, errInvalid("query parameter %q must be an integer", name)
	}
	return n, nil
}

// qList returns comma-separated and repeated values of a query parameter.
func qList(r *http.Request, name string) []string {
	var out []string
	for _, v := range r.URL.Query()[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// parseSince accepts RFC 3339 timestamps, dates (2006-01-02), or relative
// durations such as "24h", "7d", "2w".
func parseSince(v string, now time.Time) (*time.Time, error) {
	if v == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return &t, nil
	}
	if n := len(v); n > 1 {
		unit := v[n-1]
		if num, err := strconv.Atoi(v[:n-1]); err == nil && num >= 0 {
			var d time.Duration
			switch unit {
			case 'd':
				d = time.Duration(num) * 24 * time.Hour
			case 'w':
				d = time.Duration(num) * 7 * 24 * time.Hour
			}
			if d > 0 || (num == 0 && (unit == 'd' || unit == 'w')) {
				t := now.Add(-d)
				return &t, nil
			}
		}
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		t := now.Add(-d)
		return &t, nil
	}
	return nil, errInvalid("invalid time %q: use RFC 3339, YYYY-MM-DD, or a relative duration like 7d", v)
}

func qSince(r *http.Request, name string) (*time.Time, error) {
	return parseSince(r.URL.Query().Get(name), time.Now().UTC())
}

// ---------- pagination ----------
//
// Lists are ordered newest first by (created_at DESC, id DESC). Cursors are
// opaque base64url("<unix-nanos>:<id>") tokens identifying the last item of
// the previous page; this matches the encoding used by core.Store
// implementations, so cursors can be passed through to the store.

type pageParams struct {
	Limit  int
	Cursor string
	// Asc is sort_order=asc: the oldest first. The only sort_by value is
	// created_at, so the cursor format is the same in both orders.
	Asc bool
}

// listOpts returns the store options for a page fetched with limit+1 items.
func (p pageParams) listOpts() core.ListOpts {
	return core.ListOpts{Limit: p.Limit + 1, Cursor: p.Cursor, Ascending: p.Asc}
}

func pageFrom(r *http.Request) (pageParams, error) {
	limit, err := qInt(r, "limit", defaultPageSize)
	if err != nil {
		return pageParams{}, err
	}
	if limit <= 0 {
		limit = defaultPageSize
	}
	if limit > maxPageSize {
		limit = maxPageSize
	}
	c := r.URL.Query().Get("cursor")
	if c != "" {
		if _, _, err := decodeCursor(c); err != nil {
			return pageParams{}, err
		}
	}
	switch by := strings.ToLower(r.URL.Query().Get("sort_by")); by {
	case "", "created_at":
	default:
		return pageParams{}, errInvalid("unknown sort_by %q (expected created_at)", by)
	}
	pp := pageParams{Limit: limit, Cursor: c}
	switch o := strings.ToLower(r.URL.Query().Get("sort_order")); o {
	case "", "desc":
	case "asc":
		pp.Asc = true
	default:
		return pageParams{}, errInvalid("unknown sort_order %q (expected asc or desc)", o)
	}
	return pp, nil
}

func encodeCursor(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (int64, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, "", errInvalid("malformed cursor")
	}
	ns, id, ok := strings.Cut(string(b), ":")
	if !ok || id == "" {
		return 0, "", errInvalid("malformed cursor")
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return 0, "", errInvalid("malformed cursor")
	}
	return n, id, nil
}

// paginate sorts items newest first (oldest first with p.Asc) and returns
// one page plus the next cursor.
func paginate[T any](items []T, key func(T) (time.Time, string), p pageParams) ([]T, string) {
	sort.SliceStable(items, func(i, j int) bool {
		ti, ii := key(items[i])
		tj, ij := key(items[j])
		if !ti.Equal(tj) {
			return ti.After(tj) != p.Asc
		}
		return (ii > ij) != p.Asc
	})
	start := 0
	if p.Cursor != "" {
		cns, cid, _ := decodeCursor(p.Cursor)
		start = len(items)
		for i, it := range items {
			t, id := key(it)
			ns := t.UnixNano()
			next := ns < cns || (ns == cns && id < cid)
			if p.Asc {
				next = ns > cns || (ns == cns && id > cid)
			}
			if next {
				start = i
				break
			}
		}
	}
	end := start + p.Limit
	if end >= len(items) {
		return items[start:], ""
	}
	t, id := key(items[end-1])
	return items[start:end], encodeCursor(t, id)
}

// trimPage handles a store page fetched with limit+1 items.
func trimPage[T any](items []T, key func(T) (time.Time, string), limit int) ([]T, string) {
	if len(items) <= limit {
		return items, ""
	}
	items = items[:limit]
	t, id := key(items[limit-1])
	return items, encodeCursor(t, id)
}

// PageInfo is the page member of each list response.
type PageInfo struct {
	HasNextPage bool   `json:"has_next_page"`
	NextCursor  string `json:"next_cursor,omitempty"`
}

// ListResponse is the body of each list response:
// {"items": [...], "page_info": {"has_next_page", "next_cursor"}}.
type ListResponse[T any] struct {
	Items    []T      `json:"items"`
	PageInfo PageInfo `json:"page_info"`
}

// listResponse builds the list body. what names the resource for readers
// of the code only: each list has the key "items".
func listResponse[T any](what string, items []T, next string) ListResponse[T] {
	_ = what
	if items == nil {
		items = []T{}
	}
	return ListResponse[T]{Items: items, PageInfo: PageInfo{HasNextPage: next != "", NextCursor: next}}
}

// with returns the list body with extra top-level members.
func (l ListResponse[T]) with(extra map[string]any) map[string]any {
	out := map[string]any{"items": l.Items, "page_info": l.PageInfo}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// timeT shortens pagination key signatures.
type timeT = time.Time
