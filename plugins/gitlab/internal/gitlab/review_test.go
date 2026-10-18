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
)

func TestReviewOperations(t *testing.T) {
	calls := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			_ = json.Unmarshal(b, &m)
		}
		key := r.Method + " " + r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		calls[key] = m
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `{"title":"Draft: Fix it","draft":true,"diff_refs":{"base_sha":"b","head_sha":"h","start_sha":"s"}}`)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "glpat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const mr = "/api/v4/projects/acme%2Fapi/merge_requests/3"
	if err := c.ReplyInThread(ctx, "acme", "api", 3, "d1", "done"); err != nil {
		t.Fatal(err)
	}
	if calls["POST "+mr+"/discussions/d1/notes"]["body"] != "done" {
		t.Fatalf("reply calls = %v", calls)
	}
	if err := c.ResolveThread(ctx, "acme", "api", 3, "d1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := calls["PUT "+mr+"/discussions/d1?resolved=true"]; !ok {
		t.Fatalf("resolve calls = %v", calls)
	}
	err = c.SubmitReview(ctx, core.ReviewInput{Owner: "acme", Repo: "api", Number: 3, Event: core.ReviewEventApprove, Body: "LGTM",
		Comments: []core.ReviewComment{{Path: "a.go", Line: 7, Body: "nit"}}})
	if err != nil {
		t.Fatal(err)
	}
	pos := calls["POST "+mr+"/discussions"]["position"].(map[string]any)
	if pos["new_line"].(float64) != 7 || pos["head_sha"] != "h" {
		t.Fatalf("position = %v", pos)
	}
	if _, ok := calls["POST "+mr+"/approve"]; !ok {
		t.Fatal("no approval")
	}
	if err := c.SubmitReview(ctx, core.ReviewInput{Owner: "acme", Repo: "api", Number: 3, Event: core.ReviewEventRequestChanges}); !errors.Is(err, core.ErrOperationNotSupported) {
		t.Fatalf("request changes: %v", err)
	}
	if err := c.MarkReady(ctx, "acme", "api", 3); err != nil {
		t.Fatal(err)
	}
	if calls["PUT "+mr]["title"] != "Fix it" {
		t.Fatalf("mark ready = %v", calls["PUT "+mr])
	}
}

func TestDiscussionNoteHasThread(t *testing.T) {
	h := NewWebhookHandler(WebhookOptions{})
	body := `{"object_kind":"note","user":{"id":1,"username":"dana"},"project":{"path_with_namespace":"acme/api"},
		"object_attributes":{"id":9,"note":"rename","noteable_type":"MergeRequest","discussion_id":"d1","type":"DiffNote"},
		"merge_request":{"iid":3,"title":"Fix","url":"https://gl/mr/3"}}`
	evs, err := h.Normalize(core.Integration{TeamID: "t"}, "Note Hook", "x", []byte(body))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	if evs[0].Source.ThreadRef != "d1" {
		t.Fatalf("source = %+v", evs[0].Source)
	}
	// A plain note on the merge request has no thread.
	evs, _ = h.Normalize(core.Integration{TeamID: "t"}, "Note Hook", "y", []byte(strings.Replace(body, `"type":"DiffNote"`, `"type":null`, 1)))
	if evs[0].Source.ThreadRef != "" {
		t.Fatalf("source = %+v", evs[0].Source)
	}
}
