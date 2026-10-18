package orchestration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

// kvFakeStore adds a key/value store to fakeStore.
type kvFakeStore struct {
	*fakeStore
	mu sync.Mutex
	kv map[string]string
}

func (s *kvFakeStore) KVGet(_ context.Context, k string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.kv[k]
	if !ok {
		return "", core.ErrNotFound
	}
	return v, nil
}

func (s *kvFakeStore) KVSet(_ context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[k] = v
	return nil
}

func idemFixture(t *testing.T, turn func(context.Context, string) (string, error)) (*fakeEngine, *mcp.ClientSession) {
	t.Helper()
	_, base, _ := newFixture(t)
	st := &kvFakeStore{fakeStore: base, kv: map[string]string{}}
	eng := &fakeEngine{}
	srv := newServer(eng, st)
	if turn != nil {
		srv.SetTurnSource(turn)
	}
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return eng, connect(t, ts, "run_root")
}

func callRaw(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func isDuplicate(res *mcp.CallToolResult) bool {
	if len(res.Content) < 2 {
		return false
	}
	tc, ok := res.Content[len(res.Content)-1].(*mcp.TextContent)
	return ok && strings.Contains(tc.Text, `"duplicate": true`) && res.Meta["duplicate"] == true
}

func countCalls(e *fakeEngine, method string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func TestIdempotencyPerTurn(t *testing.T) {
	var mu sync.Mutex
	turn := "turn_1"
	eng, sess := idemFixture(t, func(context.Context, string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return turn, nil
	})
	args := map[string]any{"text": "hello"}

	first := callRaw(t, sess, "post_update", args)
	if first.IsError || isDuplicate(first) {
		t.Fatalf("first = %+v", first)
	}
	second := callRaw(t, sess, "post_update", args)
	if !isDuplicate(second) || second.IsError {
		t.Fatalf("second = %+v", second)
	}
	if got := second.Content[0].(*mcp.TextContent).Text; got != first.Content[0].(*mcp.TextContent).Text {
		t.Errorf("replayed text = %q", got)
	}
	if n := countCalls(eng, "PostUpdate"); n != 1 {
		t.Fatalf("PostUpdate calls = %d", n)
	}
	if res := callRaw(t, sess, "post_update", map[string]any{"text": "other"}); isDuplicate(res) {
		t.Error("different input is a duplicate")
	}
	mu.Lock()
	turn = "turn_2"
	mu.Unlock()
	if res := callRaw(t, sess, "post_update", args); isDuplicate(res) {
		t.Error("new turn is a duplicate")
	}
	if n := countCalls(eng, "PostUpdate"); n != 3 {
		t.Errorf("PostUpdate calls = %d, want 3", n)
	}
}

func TestIdempotencyWithoutTurn(t *testing.T) {
	for name, src := range map[string]func(context.Context, string) (string, error){
		"no source":   nil,
		"empty turn":  func(context.Context, string) (string, error) { return "", nil },
		"source fail": func(context.Context, string) (string, error) { return "", core.ErrConflict },
	} {
		t.Run(name, func(t *testing.T) {
			eng, sess := idemFixture(t, src)
			for i := 0; i < 2; i++ {
				if res := callRaw(t, sess, "post_update", map[string]any{"text": "x"}); isDuplicate(res) {
					t.Fatal("duplicate without a turn")
				}
			}
			if n := countCalls(eng, "PostUpdate"); n != 2 {
				t.Errorf("PostUpdate calls = %d", n)
			}
		})
	}
}

func TestIdempotencyDoesNotStoreErrors(t *testing.T) {
	eng, sess := idemFixture(t, func(context.Context, string) (string, error) { return "turn_1", nil })
	eng.err = core.ErrConflict
	args := map[string]any{"text": "x"}
	for i := 0; i < 2; i++ {
		res := callRaw(t, sess, "post_update", args)
		if !res.IsError || isDuplicate(res) {
			t.Fatalf("call %d = %+v", i, res)
		}
	}
	eng.mu.Lock()
	eng.err = nil
	eng.mu.Unlock()
	if res := callRaw(t, sess, "post_update", args); res.IsError || isDuplicate(res) {
		t.Fatalf("retry = %+v", res)
	}
	if n := countCalls(eng, "PostUpdate"); n != 3 {
		t.Errorf("PostUpdate calls = %d, want 3", n)
	}
}

func TestIdempotencyReadOnlyToolsExempt(t *testing.T) {
	_, sess := idemFixture(t, func(context.Context, string) (string, error) { return "turn_1", nil })
	for i := 0; i < 2; i++ {
		if res := callRaw(t, sess, "list_agents", nil); isDuplicate(res) {
			t.Fatal("list_agents is a duplicate")
		}
	}
}
