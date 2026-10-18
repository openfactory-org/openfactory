package mergeguard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsMergeTool(t *testing.T) {
	merge := []string{
		"merge_pull_request", "mergePullRequest", "forge.merge-pr", "merge_pr", "merge",
		"merge_merge_request", "accept_merge_request", "acceptMergeRequest", "complete_pull_request",
		"pull_request_merge", "pr_merge", "squash_merge", "enable_auto_merge", "enable_pull_request_automerge",
		"set_auto_merge", "merge_branch", "mcp__forge__merge_pull_request", "MergePR", "merge_and_close",
		"add_to_merge_queue", "enqueue_pull_request", "merge_into", "rebase_merge",
	}
	for _, n := range merge {
		if !IsMergeTool(n) {
			t.Errorf("%q must be a merge tool", n)
		}
	}
	other := []string{
		"get_merge_request", "list_merge_requests", "create_merge_request", "create_pull_request",
		"get_pull_request", "merge_request_diff", "get_merge_status", "list_merges_in_queue_status",
		"get_merge_base", "linear_get_issue", "comment_on_merge_request", "update_merge_request",
		"emerge", "submerged_items", "", "merge_request",
	}
	for _, n := range other {
		if IsMergeTool(n) {
			t.Errorf("%q must not be a merge tool", n)
		}
	}
}

func call(t *testing.T, h http.Handler, body string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec, rec.Header().Get("X-OF-Merge-Refused") == "true"
}

func TestWrapRefusesMerge(t *testing.T) {
	var seen []string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		seen = append(seen, string(b))
		w.WriteHeader(http.StatusOK)
	})
	var records []string
	allow := false
	h := Wrap(next, Policy{
		Allow: func(*http.Request) bool { return allow },
		Record: func(_ *http.Request, tool string, ok bool) {
			records = append(records, tool+map[bool]string{true: ":allowed", false: ":refused"}[ok])
		},
	})

	body := `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"merge_pull_request","arguments":{"pull_number":1}}}`
	rec, refused := call(t, h, body)
	if !refused || len(seen) != 0 {
		t.Fatalf("merge reached the server (refused=%v seen=%v)", refused, seen)
	}
	var resp struct {
		ID     int `json:"id"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.ID != 7 || !resp.Result.IsError || !strings.Contains(resp.Result.Content[0].Text, "cannot merge") {
		t.Fatalf("response = %s", rec.Body.String())
	}

	// Other calls pass unchanged, with the body intact.
	other := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"create_pull_request"}}`
	if _, refused := call(t, h, other); refused || len(seen) != 1 || seen[0] != other {
		t.Fatalf("a normal call must pass: refused=%v seen=%v", refused, seen)
	}
	list := `{"jsonrpc":"2.0","id":9,"method":"tools/list"}`
	if _, refused := call(t, h, list); refused || len(seen) != 2 {
		t.Fatal("tools/list must pass")
	}

	// A batch with a merge is refused as a whole.
	batch := `[{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_pull_request"}},` +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"enable_auto_merge"}}]`
	rec, refused = call(t, h, batch)
	if !refused || len(seen) != 2 {
		t.Fatal("a batch with a merge must be refused")
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out) != 2 || out[0]["error"] == nil || out[1]["result"] == nil {
		t.Fatalf("batch response = %s", rec.Body.String())
	}

	// With allowAgentMerge the call passes and is recorded.
	allow = true
	if _, refused := call(t, h, body); refused || len(seen) != 3 {
		t.Fatal("an allowed merge must pass")
	}
	want := []string{"merge_pull_request:refused", "enable_auto_merge:refused", "merge_pull_request:allowed"}
	if strings.Join(records, ",") != strings.Join(want, ",") {
		t.Fatalf("records = %v, want %v", records, want)
	}
}

func TestWrapPassesNonPost(t *testing.T) {
	reached := false
	h := Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }), Policy{})
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if !reached {
		t.Fatal("GET must pass")
	}
	reached = false
	_, refused := call(t, h, "not json")
	if refused || !reached {
		t.Fatal("a body that is not JSON-RPC must pass to the server, which answers it")
	}
}
