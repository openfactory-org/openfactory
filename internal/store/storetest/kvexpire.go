package storetest

import (
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// testKVExpiry checks core.KVExpirer: delete of one key, and delete of old
// keys by prefix. A store without the capability skips the test.
func testKVExpiry(t *testing.T, s core.Store) {
	x, has := s.(core.KVExpirer)
	if !has {
		t.Skip("store has no core.KVExpirer")
	}
	pfx := uid("exp") + ":"
	other := uid("exp") + ":"
	// A "_" and a "%" in a prefix are not wildcards.
	wild := pfx + "a_b%"
	for _, k := range []string{pfx + "1", pfx + "2", other + "1", wild + "x"} {
		ok(t, s.KVSet(ctx, k, "v"))
	}

	ok(t, x.KVDelete(ctx, pfx+"1"))
	ok(t, x.KVDelete(ctx, pfx+"missing"))
	_, err := s.KVGet(ctx, pfx+"1")
	isErr(t, err, core.ErrNotFound)

	// Nothing is older than a time in the past.
	n, err := x.KVDeleteOlder(ctx, pfx, time.Now().Add(-time.Hour))
	ok(t, err)
	eq(t, n, 0, "nothing expired yet")

	// A prefix with wildcards matches only itself.
	n, err = x.KVDeleteOlder(ctx, pfx+"a_b%", time.Now().Add(time.Hour))
	ok(t, err)
	eq(t, n, 1, "wildcard prefix")
	_, err = s.KVGet(ctx, pfx+"2")
	ok(t, err)

	// Everything under the prefix is older than a time in the future, and
	// other prefixes stay.
	n, err = x.KVDeleteOlder(ctx, pfx, time.Now().Add(time.Hour))
	ok(t, err)
	eq(t, n, 1, "expired by prefix")
	_, err = s.KVGet(ctx, pfx+"2")
	isErr(t, err, core.ErrNotFound)
	_, err = s.KVGet(ctx, other+"1")
	ok(t, err)
}
