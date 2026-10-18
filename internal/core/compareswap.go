package core

import "context"

// CompareAndSwapper is an optional Store capability: writes that happen only
// when the stored value is still the value that the caller read. The command
// "secret rewrap" uses it, so it does not undo a write that a live replica
// made between the read and the write.
type CompareAndSwapper interface {
	// KVCompareAndSet sets key to value when the stored value of key is old.
	// It reports false, without an error, when the key has another value or
	// does not exist.
	KVCompareAndSet(ctx context.Context, key, old, value string) (bool, error)
	// SecretCompareAndSet sets the ciphertext of the secret with id to ct
	// when its stored ciphertext is old. It reports false, without an error,
	// when the ciphertext changed or the secret does not exist.
	SecretCompareAndSet(ctx context.Context, id string, old, ct []byte) (bool, error)
}
