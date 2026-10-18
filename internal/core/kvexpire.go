package core

import (
	"context"
	"time"
)

// KVExpirer is an optional Store capability: delete of key/value rows. The
// store keeps the time of the last write of each row, so old rows can expire.
// A store without it keeps its rows forever.
type KVExpirer interface {
	// KVDelete removes key. A key that does not exist is not an error.
	KVDelete(ctx context.Context, key string) error
	// KVDeleteOlder removes the rows whose key starts with prefix and whose
	// last write is before the given time. It returns the number of rows.
	KVDeleteOlder(ctx context.Context, prefix string, before time.Time) (int, error)
}
