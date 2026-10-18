package core

import "context"

// KeyProvider wraps and unwraps the data keys of the secret store. The
// built-in provider keeps its keys in memory (OF_SECRET_KEY). A provider for a
// key management service can live in another module and replace it with
// the WithKeyProvider option.
//
// A provider must return an error that wraps ErrNotFound when it has no key
// for a version. The empty version means "unknown". The secret store passes it
// for data keys that were written before key versions existed. A provider can
// try each key it has, or return an error.
type KeyProvider interface {
	// CurrentVersion names the key for new writes.
	CurrentVersion() string
	// Wrap encrypts a data key with the current key and returns the version used.
	Wrap(ctx context.Context, dataKey []byte) (wrapped []byte, version string, err error)
	// Unwrap decrypts a data key that was wrapped with the named version.
	Unwrap(ctx context.Context, wrapped []byte, version string) ([]byte, error)
}
