package webhooks

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
)

// sharedKVStore is the database that the replicas share: the fake webhook
// records plus a real store for the KV and the coordination locks.
type sharedKVStore struct {
	*fakeStore
	core.Coordinator
	kv core.Store
}

func (s *sharedKVStore) KVGet(ctx context.Context, key string) (string, error) {
	return s.kv.KVGet(ctx, key)
}

func (s *sharedKVStore) KVSet(ctx context.Context, key, value string) error {
	return s.kv.KVSet(ctx, key, value)
}

type kvCoordStore interface {
	core.Store
	core.Coordinator
}

func sharedStores(t *testing.T) map[string]kvCoordStore {
	s, err := sqlite.OpenStore(context.Background(), filepath.Join(t.TempDir(), "of.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return map[string]kvCoordStore{"sqlite": s}
}

func TestRateLimitHoldsAcrossTwoReplicas(t *testing.T) {
	for name, real := range sharedStores(t) {
		t.Run(name, func(t *testing.T) {
			base := newEnv(t, Options{})
			st := &sharedKVStore{fakeStore: base.store, Coordinator: real, kv: real}
			now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
			var clockMu sync.Mutex
			clock := func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
			var muxes []*http.ServeMux
			for i := 0; i < 2; i++ { // two replicas on one database
				h := NewHandler(st, base.secrets, base.bus, Options{Now: clock})
				mux := http.NewServeMux()
				h.Register(mux)
				muxes = append(muxes, mux)
			}
			e := &env{store: base.store, secrets: base.secrets, bus: base.bus}
			var mu sync.Mutex
			accepted, limited := 0, 0
			var wg sync.WaitGroup
			for i := 0; i < 2*DefaultRateLimit; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					e := *e
					e.mux = muxes[i%2]
					rr := e.do("POST", "/webhooks/w/wh_tok", fmt.Sprintf(`{"i":%d}`, i), http.Header{
						"Authorization": {"Bearer whsec_token-value"}, "X-Ci-Run-Id": {fmt.Sprint("run-", i)}})
					mu.Lock()
					defer mu.Unlock()
					switch rr.Code {
					case http.StatusAccepted:
						accepted++
					case http.StatusTooManyRequests:
						limited++
					default:
						t.Errorf("delivery %d: status %d %s", i, rr.Code, rr.Body.String())
					}
				}(i)
			}
			wg.Wait()
			if accepted != DefaultRateLimit || limited != DefaultRateLimit {
				t.Fatalf("accepted %d, limited %d; want %d each", accepted, limited, DefaultRateLimit)
			}

			// A new window accepts deliveries again, on both replicas.
			clockMu.Lock()
			now = now.Add(time.Minute)
			clockMu.Unlock()
			for i := 0; i < 2; i++ {
				e := *e
				e.mux = muxes[i]
				if rr := e.do("POST", "/webhooks/w/wh_tok", fmt.Sprintf(`{"next":%d}`, i), http.Header{
					"Authorization": {"Bearer whsec_token-value"}, "X-Ci-Run-Id": {fmt.Sprint("next-", i)}}); rr.Code != http.StatusAccepted {
					t.Fatalf("new window, replica %d: status %d", i, rr.Code)
				}
			}
		})
	}
}

func TestSharedWindowLimiterFallsBackToMemory(t *testing.T) {
	// A store that is not a coordinator counts in process memory.
	l := newSharedWindowLimiter(&fakeStore{}, RateLimitKVPrefix, 2, time.Minute, nil)
	if l.shared != nil {
		t.Fatal("limiter must not share without a coordinator")
	}
	now := time.Now()
	for i := 0; i < 2; i++ {
		if ok, _ := l.allow("wh", now); !ok {
			t.Fatalf("allow %d refused", i)
		}
	}
	if ok, retry := l.allow("wh", now); ok || retry != time.Minute {
		t.Fatalf("third allow = %v, retry %v", ok, retry)
	}
}

func TestParseWindow(t *testing.T) {
	start := time.Unix(0, 1_700_000_000_123_456_789)
	s, n, ok := parseWindow(formatWindow(start, 7))
	if !ok || !s.Equal(start) || n != 7 {
		t.Fatalf("parse = %v %d %v", s, n, ok)
	}
	for _, bad := range []string{"", "x", "1 y", "y 1"} {
		if _, _, ok := parseWindow(bad); ok {
			t.Fatalf("parsed %q", bad)
		}
	}
}
