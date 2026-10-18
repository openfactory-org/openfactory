package eventctx

import (
	"context"
	"errors"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Retention is how long event context stays in the key/value store when no
// run end removes it first. The leader removes older keys. It is a backstop
// for runs that ended without a release, and for context of runs that no one
// ever started.
const Retention = 7 * 24 * time.Hour

// KeyPrefix is the prefix of each key/value store key of this package.
const KeyPrefix = "eventctx:"

// Release removes the event context that the run used: the context of each
// message of the run, and the key that names its first prompt. It returns
// the number of keys it asked the store to remove. A store without core.KVExpirer keeps the
// keys, and Release does nothing.
//
// The caller calls Release when the run ends. A run that ended can get a
// new message: that message stores its own context.
func Release(ctx context.Context, st core.Store, runID string) (int, error) {
	kv, ok := st.(core.KVExpirer)
	if !ok || runID == "" {
		return 0, nil
	}
	keys := map[string]bool{}
	promptKey := PromptKVKey(runID)
	switch id, err := st.KVGet(ctx, promptKey); {
	case err == nil:
		keys[promptKey] = true
		if id != "" {
			keys[KVKey(id)] = true
		}
	case !errors.Is(err, core.ErrNotFound):
		return 0, err
	}
	msgs, err := st.ListRunMessages(ctx, runID, 0)
	if err != nil {
		return 0, err
	}
	for _, m := range msgs {
		if m.Role == "user" {
			keys[KVKey(m.ID)] = true
		}
	}
	n := 0
	for k := range keys {
		if err := kv.KVDelete(ctx, k); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Expire removes the keys that were written before now minus Retention. It
// returns the number of keys. A store without core.KVExpirer keeps them.
func Expire(ctx context.Context, st core.Store, now time.Time) (int, error) {
	kv, ok := st.(core.KVExpirer)
	if !ok {
		return 0, nil
	}
	return kv.KVDeleteOlder(ctx, KeyPrefix, now.Add(-Retention))
}
