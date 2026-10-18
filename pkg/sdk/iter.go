package sdk

import (
	"context"
	"iter"
	"net/url"
	"strconv"
)

// Iter lazily walks every page of a list endpoint.
//
//	it := client.Runs.List(ctx, nil)
//	for it.Next(ctx) {
//		run := it.Value()
//	}
//	if err := it.Err(); err != nil { ... }
type Iter[T any] struct {
	fetch  func(ctx context.Context, cursor string) ([]T, string, error)
	buf    []T
	cursor string
	cur    T
	err    error
	done   bool
	start  bool
}

func newIter[T any](fetch func(ctx context.Context, cursor string) ([]T, string, error)) *Iter[T] {
	return &Iter[T]{fetch: fetch}
}

// Next advances to the next item, fetching pages as needed.
func (it *Iter[T]) Next(ctx context.Context) bool {
	for len(it.buf) == 0 {
		if it.err != nil || it.done {
			return false
		}
		if it.start && it.cursor == "" {
			it.done = true
			return false
		}
		it.start = true
		items, next, err := it.fetch(ctx, it.cursor)
		if err != nil {
			it.err = err
			return false
		}
		it.buf, it.cursor = items, next
		if len(items) == 0 && next == "" {
			it.done = true
			return false
		}
	}
	it.cur, it.buf = it.buf[0], it.buf[1:]
	return true
}

// Value returns the current item.
func (it *Iter[T]) Value() T { return it.cur }

// Err returns the first error encountered.
func (it *Iter[T]) Err() error { return it.err }

// All adapts the iterator to a range-over-func sequence. Iteration stops after
// yielding a non-nil error.
func (it *Iter[T]) All(ctx context.Context) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		for it.Next(ctx) {
			if !yield(it.Value(), nil) {
				return
			}
		}
		if it.err != nil {
			var zero T
			yield(zero, it.err)
		}
	}
}

// Collect drains the iterator into a slice.
func (it *Iter[T]) Collect(ctx context.Context) ([]T, error) {
	var out []T
	for it.Next(ctx) {
		out = append(out, it.Value())
	}
	return out, it.err
}

// PageInfo is the page member of each list response.
type PageInfo struct {
	HasNextPage bool   `json:"has_next_page"`
	NextCursor  string `json:"next_cursor,omitempty"`
}

// ListPage is the body of each list response:
// {"items": [...], "page_info": {...}}.
type ListPage[T any] struct {
	Items    []T      `json:"items"`
	PageInfo PageInfo `json:"page_info"`
}

// listIter builds an iterator over GET path. Each list response has the
// items under "items"; key names the resource for readers only.
func listIter[T any](c *Client, path, key string, q url.Values, pageSize int) *Iter[T] {
	_ = key
	return newIter(func(ctx context.Context, cursor string) ([]T, string, error) {
		qq := url.Values{}
		for k, v := range q {
			qq[k] = v
		}
		if pageSize > 0 {
			qq.Set("limit", strconv.Itoa(pageSize))
		}
		if cursor != "" {
			qq.Set("cursor", cursor)
		}
		var page ListPage[T]
		if err := c.Do(ctx, "GET", path, qq, nil, &page); err != nil {
			return nil, "", err
		}
		return page.Items, page.PageInfo.NextCursor, nil
	})
}

// listAll fetches an unpaginated list endpoint.
func listAll[T any](ctx context.Context, c *Client, path, key string, q url.Values) ([]T, error) {
	return listIter[T](c, path, key, q, 0).Collect(ctx)
}
