package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
)

func TestReviewThreadAndFileDiff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/acme%2Fapi/merge_requests/3/discussions/d1":
			fmt.Fprint(w, `{"id":"d1","notes":[
				{"id":10,"body":"rename this","created_at":"2026-01-02T03:04:05Z","resolvable":true,"resolved":true,"author":{"username":"dana"},
				 "position":{"new_path":"pkg/x.go","new_line":7}},
				{"id":11,"body":"assigned to someone","system":true,"author":{"username":"gitlab"}},
				{"id":12,"body":"done","resolvable":true,"resolved":true,"author":{"username":"bot"}}]}`)
		case "/api/v4/projects/acme%2Fapi/merge_requests/3/diffs":
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-Next-Page", "2")
				fmt.Fprint(w, `[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-a\n+b\n"}]`)
				return
			}
			fmt.Fprint(w, `[{"old_path":"pkg/x.go","new_path":"pkg/x.go","diff":"@@ -7 +7 @@\n-old\n+new\n"},
				{"old_path":"big.go","new_path":"big.go","diff":"","too_large":true}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "glpat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	th, err := c.ReviewThread(ctx, "acme", "api", 3, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if th.Path != "pkg/x.go" || th.Line != 7 || !th.Resolved || len(th.Comments) != 2 || th.Comments[0].Author != "dana" || th.Comments[0].CreatedAt.IsZero() {
		t.Fatalf("thread = %+v", th)
	}
	// The diff is on the second page.
	d, err := c.FileDiff(ctx, "acme", "api", 3, "pkg/x.go")
	if err != nil || !strings.Contains(d.Patch, "+new") {
		t.Fatalf("diff = %+v, %v", d, err)
	}
	if _, err := c.FileDiff(ctx, "acme", "api", 3, "big.go"); !errors.Is(err, core.ErrNotFound) || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("large file: %v", err)
	}
	if _, err := c.FileDiff(ctx, "acme", "api", 3, "nope.go"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unchanged file: %v", err)
	}
	if _, err := c.ReviewThread(ctx, "acme", "api", 3, ""); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("empty ref: %v", err)
	}
}

func TestFileDiffOldGitLab(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/acme%2Fapi/merge_requests/3/changes":
			fmt.Fprint(w, `{"changes":[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-a\n+b\n"}]}`)
		default:
			http.NotFound(w, r) // no /diffs call before GitLab 15.7
		}
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	d, err := c.FileDiff(context.Background(), "acme", "api", 3, "a.go")
	if err != nil || !strings.Contains(d.Patch, "+b") {
		t.Fatalf("diff = %+v, %v", d, err)
	}
}

// TestRequestReviewers shows that the reviewers are added to the present ones
// (GitLab replaces them on update) and that the user is found by username.
func TestRequestReviewers(t *testing.T) {
	var put map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v4/user":
			fmt.Fprint(w, `{"id":5,"username":"alice"}`)
		case r.URL.Path == "/api/v4/users" && r.URL.Query().Get("username") == "alice":
			fmt.Fprint(w, `[{"id":5,"username":"alice"},{"id":6,"username":"alice2"}]`)
		case r.URL.Path == "/api/v4/users":
			fmt.Fprint(w, `[]`)
		case r.URL.EscapedPath() == "/api/v4/projects/acme%2Fapi/merge_requests/3" && r.Method == http.MethodGet:
			fmt.Fprint(w, `{"reviewers":[{"id":9}]}`)
		case r.URL.EscapedPath() == "/api/v4/projects/acme%2Fapi/merge_requests/3" && r.Method == http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &put)
			fmt.Fprint(w, `{}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	ctx := context.Background()
	if l, err := c.Login(ctx); err != nil || l != "alice" {
		t.Fatalf("login = %q, %v", l, err)
	}
	if err := c.RequestReviewers(ctx, "acme", "api", 3, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(put["reviewer_ids"]) != "[9 5]" {
		t.Fatalf("reviewer_ids = %v", put["reviewer_ids"])
	}
	put = nil
	if err := c.RequestReviewers(ctx, "acme", "api", 3, []string{"nobody"}); !errors.Is(err, core.ErrNotFound) || put != nil {
		t.Fatalf("unknown user: %v, put = %v", err, put)
	}
}

// TestNormalizeActionKey shows that the events of one delivery share an
// action key.
func TestNormalizeActionKey(t *testing.T) {
	h := NewWebhookHandler(WebhookOptions{})
	body := `{"object_kind":"note","user":{"id":1,"username":"dana"},"project":{"path_with_namespace":"acme/api"},
		"object_attributes":{"id":9,"note":"hi @payments","noteable_type":"MergeRequest","discussion_id":"d1","type":"DiffNote"},
		"merge_request":{"iid":3,"title":"Fix","url":"https://gl/mr/3"}}`
	evs, err := h.Normalize(core.Integration{TeamID: "t"}, "Note Hook", "uuid-1", []byte(body))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	for _, ev := range evs {
		if ev.ActionKey != "gitlab:uuid-1" || ev.DeliveryID != "uuid-1:"+ev.Type {
			t.Errorf("%s: action key = %q, delivery = %q", ev.Type, ev.ActionKey, ev.DeliveryID)
		}
	}
	evs, _ = h.Normalize(core.Integration{TeamID: "t"}, "Note Hook", "", []byte(body))
	if evs[0].ActionKey != "" {
		t.Errorf("a delivery without an ID has the action key %q", evs[0].ActionKey)
	}
}

type oneForge struct{ f core.Forge }

func (o oneForge) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return o.f, nil }

// TestReviewBuilderWithGitLab runs the context builder of a review comment
// against a fake GitLab: it writes the thread and the diff of the file.
func TestReviewBuilderWithGitLab(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/api/v4/projects/acme%2Fapi/merge_requests/3/discussions/d1":
			fmt.Fprint(w, `{"id":"d1","notes":[{"id":10,"body":"rename this","author":{"username":"dana"},"position":{"new_path":"pkg/x.go","new_line":7}}]}`)
		case "/api/v4/projects/acme%2Fapi/merge_requests/3/diffs":
			fmt.Fprint(w, `[{"old_path":"pkg/x.go","new_path":"pkg/x.go","diff":"@@ -7 +7 @@\n-old\n+new\n"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: "t"})
	h := NewWebhookHandler(WebhookOptions{})
	body := `{"object_kind":"note","user":{"id":1,"username":"dana"},"project":{"path_with_namespace":"acme/api"},
		"object_attributes":{"id":10,"note":"rename this","noteable_type":"MergeRequest","discussion_id":"d1","type":"DiffNote",
		"position":{"new_path":"pkg/x.go","new_line":7}},
		"merge_request":{"iid":3,"title":"Fix","url":"https://gl/mr/3"}}`
	evs, err := h.Normalize(core.Integration{TeamID: "t"}, "Note Hook", "uuid-1", []byte(body))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	b := eventctx.Build(context.Background(), "m", evs[0], true, eventctx.ReviewBuilders(oneForge{c}))
	files := map[string]string{}
	for _, f := range b.Files {
		files[f.Path] = string(f.Data)
	}
	const dir = ".openfactory/events/m/context/"
	if !strings.Contains(files[dir+"review_thread.json"], "rename this") || !strings.Contains(files[dir+"file_diff.patch"], "+new") {
		t.Fatalf("files = %v, omitted = %+v", b.Envelope.ContextFiles, b.Envelope.Omitted)
	}
	if len(b.Envelope.Omitted) != 0 {
		t.Fatalf("omitted = %+v", b.Envelope.Omitted)
	}
}
