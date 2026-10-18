// Package storeutil holds helpers shared by every core.Store implementation:
// opaque pagination cursors, limit normalization, worker-host normalization and
// the optional retention interface.
package storeutil

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

const (
	// DefaultLimit is applied when ListOpts.Limit <= 0.
	DefaultLimit = 50
	// MaxLimit caps ListOpts.Limit.
	MaxLimit = 500
	// ServerWorkerHost is the default worker host; runs with an empty
	// Config.WorkerHost are claimable by workerHost "" or "server".
	ServerWorkerHost = "server"
)

// Limit normalizes a requested page size.
func Limit(n int) int {
	switch {
	case n <= 0:
		return DefaultLimit
	case n > MaxLimit:
		return MaxLimit
	default:
		return n
	}
}

// Cursor identifies a position in a list ordered by (created_at DESC, id DESC).
type Cursor struct {
	CreatedNS int64
	ID        string
}

// EncodeCursor returns the opaque cursor that continues a listing after the
// item with the given creation time and ID.
func EncodeCursor(createdAt time.Time, id string) string {
	raw := strconv.FormatInt(createdAt.UnixNano(), 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor parses an opaque cursor. An empty string yields (nil, nil).
func DecodeCursor(s string) (*Cursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed cursor", core.ErrInvalid)
	}
	ns, id, ok := strings.Cut(string(b), ":")
	if !ok || id == "" {
		return nil, fmt.Errorf("%w: malformed cursor", core.ErrInvalid)
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: malformed cursor", core.ErrInvalid)
	}
	return &Cursor{CreatedNS: n, ID: id}, nil
}

// After reports whether an item at (createdNS, id) comes after the cursor in
// (created DESC, id DESC) order, i.e. belongs to the next page.
func (c *Cursor) After(createdNS int64, id string) bool {
	if c == nil {
		return true
	}
	return createdNS < c.CreatedNS || (createdNS == c.CreatedNS && id < c.ID)
}

// AfterAsc is After for the (created ASC, id ASC) order.
func (c *Cursor) AfterAsc(createdNS int64, id string) bool {
	if c == nil {
		return true
	}
	return createdNS > c.CreatedNS || (createdNS == c.CreatedNS && id > c.ID)
}

// WorkerHost normalizes a worker host: empty means "server".
func WorkerHost(h string) string {
	if h == "" {
		return ServerWorkerHost
	}
	return h
}

// SourceKey returns the key used to find a work item by its source.
func SourceKey(w *core.WorkItem) string {
	if w.SourceKey != "" {
		return w.SourceKey
	}
	return w.Source.Key()
}

// NS converts an optional time to unix nanoseconds (0 when nil or zero).
func NS(t *time.Time) int64 {
	if t == nil || t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

// Now returns the current time in UTC. Stores use it for defaulted timestamps.
func Now() time.Time { return time.Now().UTC() }

// Pruner is implemented by stores that support retention cleanup.
type Pruner interface {
	// PruneBefore deletes terminal runs (and their messages) finished before
	// cutoff, resolved inbox items resolved before cutoff and webhook
	// deliveries received before cutoff. It returns the number of deleted rows.
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
}
