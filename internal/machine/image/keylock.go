package image

import "sync"

// KeyedMutex is a set of mutexes, one per key. An entry exists only while
// a caller holds or waits for its key, so the map does not grow with every
// key that was ever used.
type KeyedMutex struct {
	mu      sync.Mutex
	entries map[string]*keyEntry
}

type keyEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *KeyedMutex) ref(key string) *keyEntry {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.entries == nil {
		k.entries = map[string]*keyEntry{}
	}
	e, ok := k.entries[key]
	if !ok {
		e = &keyEntry{}
		k.entries[key] = e
	}
	e.refs++
	return e
}

func (k *KeyedMutex) unref(key string, e *keyEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.refs--
	if e.refs == 0 {
		delete(k.entries, key)
	}
}

// Lock locks key and returns its unlock function.
func (k *KeyedMutex) Lock(key string) func() {
	e := k.ref(key)
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.unref(key, e)
	}
}

// TryLock locks key when no other caller holds it. It reports false, and
// locks nothing, when the key is busy.
func (k *KeyedMutex) TryLock(key string) (func(), bool) {
	e := k.ref(key)
	if !e.mu.TryLock() {
		k.unref(key, e)
		return nil, false
	}
	return func() {
		e.mu.Unlock()
		k.unref(key, e)
	}, true
}

// Len returns the number of keys that are held or waited for.
func (k *KeyedMutex) Len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}
