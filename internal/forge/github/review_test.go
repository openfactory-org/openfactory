package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

type recorded struct {
	Method, Path string
	Body         map[string]any
}

func reviewServer(t *testing.T, handle func(r recorded) any) (*Client, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec := recorded{Method: r.Method, Path: r.URL.Path}
		_ = json.Unmarshal(b, &rec.Body)
		mu.Lock()
		calls = append(calls, rec)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		out := handle(rec)
		if out == nil {
			out = map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{BaseURL: srv.URL, Token: "ghp_pat"})
	if err != nil {
		t.Fatal(err)
	}
	return c, &calls
}

func TestReplyInThreadAndReview(t *testing.T) {
	c, calls := reviewServer(t, func(recorded) any { return nil })
	ctx := context.Background()
	if err := c.ReplyInThread(ctx, "acme", "api", 4, "9001", "Done"); err != nil {
		t.Fatal(err)
	}
	if err := c.ReplyInThread(ctx, "acme", "api", 4, "not-a-number", "x"); err == nil {
		t.Fatal("bad thread ref accepted")
	}
	err := c.SubmitReview(ctx, core.ReviewInput{Owner: "acme", Repo: "api", Number: 4, Event: core.ReviewEventRequestChanges, Body: "See notes",
		Comments: []core.ReviewComment{{Path: "main.go", Line: 12, Body: "nil check"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := *calls
	if len(got) != 2 || got[0].Path != "/repos/acme/api/pulls/4/comments/9001/replies" || got[0].Body["body"] != "Done" {
		t.Fatalf("reply call = %+v", got)
	}
	if got[1].Path != "/repos/acme/api/pulls/4/reviews" || got[1].Body["event"] != "REQUEST_CHANGES" {
		t.Fatalf("review call = %+v", got[1])
	}
	cm := got[1].Body["comments"].([]any)[0].(map[string]any)
	if cm["path"] != "main.go" || cm["line"].(float64) != 12 || cm["side"] != "RIGHT" {
		t.Fatalf("inline comment = %+v", cm)
	}
	if err := c.SubmitReview(ctx, core.ReviewInput{Owner: "acme", Repo: "api", Number: 4, Comments: []core.ReviewComment{{Path: "x"}}}); err == nil {
		t.Fatal("comment without a line accepted")
	}
}

func TestResolveThreadAndMarkReady(t *testing.T) {
	c, calls := reviewServer(t, func(r recorded) any {
		switch {
		case r.Path == "/graphql" && strings.Contains(r.Body["query"].(string), "reviewThreads"):
			return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{"reviewThreads": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": false},
				"nodes": []any{
					map[string]any{"id": "PRRT_a", "isResolved": false, "comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 1}}}},
					map[string]any{"id": "PRRT_b", "isResolved": false, "comments": map[string]any{"nodes": []any{map[string]any{"databaseId": 9001}}}},
				}}}}}}
		case r.Path == "/graphql":
			return map[string]any{"data": map[string]any{}}
		case r.Method == http.MethodGet:
			return map[string]any{"node_id": "PR_node", "draft": true}
		}
		return nil
	})
	ctx := context.Background()
	if err := c.ResolveThread(ctx, "acme", "api", 4, "9001"); err != nil {
		t.Fatal(err)
	}
	if err := c.MarkReady(ctx, "acme", "api", 4); err != nil {
		t.Fatal(err)
	}
	got := *calls
	if len(got) != 4 {
		t.Fatalf("calls = %+v", got)
	}
	if v := got[1].Body["variables"].(map[string]any); v["id"] != "PRRT_b" || !strings.Contains(got[1].Body["query"].(string), "resolveReviewThread") {
		t.Fatalf("resolve = %+v", got[1])
	}
	if v := got[3].Body["variables"].(map[string]any); v["id"] != "PR_node" || !strings.Contains(got[3].Body["query"].(string), "markPullRequestReadyForReview") {
		t.Fatalf("mark ready = %+v", got[3])
	}
}

func TestGraphQLPathOnEnterpriseServer(t *testing.T) {
	c, err := New(Config{BaseURL: "https://ghe.example.com/api/v3", Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if p := c.graphqlPath(); p != "https://ghe.example.com/api/graphql" {
		t.Fatalf("path = %q", p)
	}
}

func TestReviewCommentEventHasThread(t *testing.T) {
	body := `{"action":"created","repository":{"full_name":"acme/api"},"sender":{"id":5,"login":"rev"},
		"pull_request":{"number":4,"title":"Fix","html_url":"https://github.com/acme/api/pull/4","user":{"login":"of[bot]","type":"Bot"},
			"additions":10,"deletions":3,"created_at":"2026-11-01T10:00:00Z","head":{"ref":"fix"},"base":{"ref":"main"}},
		"comment":{"id":9002,"in_reply_to_id":9001,"body":"Rename this","user":{"login":"rev"}}}`
	evs, err := Normalizer{}.Normalize("pull_request_review_comment", "d1", []byte(body))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v, err = %v", evs, err)
	}
	ev := evs[0]
	if ev.Source.ThreadRef != "9001" || ev.Source.Key() != "github:acme/api#4" {
		t.Fatalf("source = %+v", ev.Source)
	}
	if ev.Attributes["additions"] != 10 || ev.Attributes["deletions"] != 3 || ev.Attributes["pr_opener_is_bot"] != true {
		t.Fatalf("attributes = %+v", ev.Attributes)
	}
	// The first comment of a thread names the thread itself.
	body = strings.Replace(body, `"in_reply_to_id":9001,`, "", 1)
	evs, _ = Normalizer{}.Normalize("pull_request_review_comment", "d2", []byte(body))
	if evs[0].Source.ThreadRef != "9002" {
		t.Fatalf("thread = %q", evs[0].Source.ThreadRef)
	}
}
