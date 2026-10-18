package sqlstore

import (
	"context"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

var _ core.CompareAndSwapper = (*Store)(nil)

// KVCompareAndSet implements core.CompareAndSwapper.
func (s *Store) KVCompareAndSet(ctx context.Context, key, old, value string) (bool, error) {
	return s.swapOne(ctx, "UPDATE kv SET value=?, updated_ns=? WHERE key=? AND value=?", value, time.Now().UnixNano(), key, old)
}

// SecretCompareAndSet implements core.CompareAndSwapper. It changes only the
// ciphertext. The metadata of the secret stays.
func (s *Store) SecretCompareAndSet(ctx context.Context, id string, old, ct []byte) (bool, error) {
	return s.swapOne(ctx, "UPDATE secrets SET ciphertext=? WHERE id=? AND ciphertext=?", ct, id, old)
}

// swapOne runs a conditional update and reports whether it changed a row.
func (s *Store) swapOne(ctx context.Context, query string, args ...any) (bool, error) {
	res, err := s.exec(ctx, s.conn(ctx), query, args...)
	if err != nil {
		return false, err
	}
	n, err := s.rowsAffected(res)
	return n > 0, err
}
