package storetest

import (
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// testCompareAndSwap checks core.CompareAndSwapper: a write happens only when
// the stored value is the old value. A store without the capability skips
// the test.
func testCompareAndSwap(t *testing.T, s core.Store) {
	x, has := s.(core.CompareAndSwapper)
	if !has {
		t.Skip("store has no core.CompareAndSwapper")
	}

	key := uid("cas")
	ok(t, s.KVSet(ctx, key, "a"))
	set, err := x.KVCompareAndSet(ctx, key, "other", "b")
	ok(t, err)
	eq(t, set, false, "kv swap from a wrong old value")
	v, _ := s.KVGet(ctx, key)
	eq(t, v, "a", "kv value after a refused swap")
	set, err = x.KVCompareAndSet(ctx, key, "a", "b")
	ok(t, err)
	eq(t, set, true, "kv swap")
	v, _ = s.KVGet(ctx, key)
	eq(t, v, "b", "kv value after a swap")
	// A key that was deleted (set to "") or never written does not come back.
	ok(t, s.KVSet(ctx, key, ""))
	set, err = x.KVCompareAndSet(ctx, key, "b", "c")
	ok(t, err)
	eq(t, set, false, "kv swap of a deleted value")
	set, err = x.KVCompareAndSet(ctx, uid("cas"), "", "c")
	ok(t, err)
	eq(t, set, false, "kv swap of a missing key")

	tm := mkTeam(t, s)
	sec := &core.Secret{TeamID: tm.ID, Name: "CAS", Type: core.SecretGeneric}
	ok(t, s.PutSecret(ctx, sec, []byte{0, 1, 255}))
	set, err = x.SecretCompareAndSet(ctx, sec.ID, []byte("other"), []byte("new"))
	ok(t, err)
	eq(t, set, false, "secret swap from a wrong old value")
	set, err = x.SecretCompareAndSet(ctx, sec.ID, []byte{0, 1, 255}, []byte{9, 0, 9})
	ok(t, err)
	eq(t, set, true, "secret swap")
	got, ct, err := s.GetSecret(ctx, tm.ID, "", "CAS")
	ok(t, err)
	eq(t, string(ct), string([]byte{9, 0, 9}), "secret ciphertext after a swap")
	eq(t, got.Type, core.SecretGeneric, "secret type stays")
	set, err = x.SecretCompareAndSet(ctx, uid("sec"), []byte{9, 0, 9}, []byte("x"))
	ok(t, err)
	eq(t, set, false, "secret swap of a missing id")
}
