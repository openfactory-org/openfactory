package integrations

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Timing of the name lists of a core.NameResolver. A list is read
// again when it is older than ListTTL. A name that is not in the list gives
// one more read, but not more often than MinRefresh (an object that was just
// created is found, and a typing error does not call the API on each check).
// After an error the resolver does not call the API again for ErrorBackoff,
// and it uses the old list for up to StaleMax.
const (
	ListTTL      = 5 * time.Minute
	MinRefresh   = 30 * time.Second
	ErrorBackoff = 30 * time.Second
	StaleMax     = time.Hour
)

// NameLists caches the name lists of the resolvers of one provider. A list
// maps a lower-case name to the IDs that carry it. The Slack resolver has
// the same rules.
type NameLists struct {
	// Now overrides the clock (tests).
	Now func() time.Time

	mu    sync.Mutex
	lists map[string]*nameList
}

// nameList is one cached list. mu is held while the list is read.
type nameList struct {
	mu      sync.Mutex
	byName  map[string][]string
	fetched time.Time
	err     error
	errAt   time.Time
}

func (n *NameLists) now() time.Time {
	if n.Now != nil {
		return n.Now()
	}
	return time.Now()
}

// Get returns the list for key. It reads the list with fetch when the cache
// is older than maxAge. After a failed read, it returns the old list (up to
// StaleMax old) or the error, without a new call for ErrorBackoff.
func (n *NameLists) Get(ctx context.Context, key string, maxAge time.Duration, fetch func(context.Context) (map[string][]string, error)) (map[string][]string, error) {
	n.mu.Lock()
	if n.lists == nil {
		n.lists = map[string]*nameList{}
	}
	l := n.lists[key]
	if l == nil {
		l = &nameList{}
		n.lists[key] = l
	}
	n.mu.Unlock()

	l.mu.Lock()
	defer l.mu.Unlock()
	now := n.now()
	if l.byName != nil && now.Sub(l.fetched) < maxAge {
		return l.byName, nil
	}
	if l.err != nil && now.Sub(l.errAt) < ErrorBackoff {
		return l.stale(now, l.err)
	}
	byName, err := fetch(ctx)
	if err != nil {
		l.err, l.errAt = err, now
		return l.stale(now, err)
	}
	l.byName, l.fetched, l.err = byName, now, nil
	return byName, nil
}

// Lookup is Get with the rules of a name check: it reads the list again once
// when a wanted key is missing and the list is older than MinRefresh.
func (n *NameLists) Lookup(ctx context.Context, key string, keys []string, fetch func(context.Context) (map[string][]string, error)) (map[string][]string, error) {
	byName, err := n.Get(ctx, key, ListTTL, fetch)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if _, found := byName[k]; !found {
			if again, err := n.Get(ctx, key, MinRefresh, fetch); err == nil {
				return again, nil
			}
			break
		}
	}
	return byName, nil
}

// stale returns the old list when it is not too old, and err otherwise.
func (l *nameList) stale(now time.Time, err error) (map[string][]string, error) {
	if l.byName != nil && now.Sub(l.fetched) < StaleMax {
		return l.byName, nil
	}
	return nil, err
}

// AddName adds id to the entry of name, in lower case and trimmed. An empty
// name or ID and a duplicate ID are left out.
func AddName(m map[string][]string, name, id string) {
	if k := strings.ToLower(strings.TrimSpace(name)); k != "" && id != "" {
		AddID(m, k, id)
	}
}

// AddID adds id to the entry of key, unless it is there already.
func AddID(m map[string][]string, key, id string) {
	for _, x := range m[key] {
		if x == id {
			return
		}
	}
	m[key] = append(m[key], id)
}

// SortIDs sorts the IDs of each name.
func SortIDs(m map[string][]string) {
	for n := range m {
		sort.Strings(m[n])
	}
}
