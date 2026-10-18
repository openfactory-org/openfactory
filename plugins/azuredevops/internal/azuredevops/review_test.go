package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
		calls[r.Method+" "+r.URL.Path] = m
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()
	c, err := New(Config{Organization: "contoso", BaseURL: srv.URL, Token: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const pr = "/contoso/proj/_apis/git/repositories/web/pullRequests/11"
	if err := c.ReplyInThread(ctx, "proj", "web", 11, "66", "done"); err != nil {
		t.Fatal(err)
	}
	if got := calls["POST "+pr+"/threads/66/comments"]; got["content"] != "done" {
		t.Fatalf("calls = %v", calls)
	}
	if err := c.ResolveThread(ctx, "proj", "web", 11, "66"); err != nil {
		t.Fatal(err)
	}
	if calls["PATCH "+pr+"/threads/66"]["status"] != "fixed" {
		t.Fatalf("calls = %v", calls)
	}
	if err := c.ReplyInThread(ctx, "proj", "web", 11, "x", "done"); err == nil {
		t.Fatal("bad thread accepted")
	}
	if err := c.SubmitReview(ctx, core.ReviewInput{Owner: "proj", Repo: "web", Number: 11, Comments: []core.ReviewComment{{Path: "a.go", Line: 3, Body: "nit"}}}); err != nil {
		t.Fatal(err)
	}
	tc := calls["POST "+pr+"/threads"]["threadContext"].(map[string]any)
	if tc["filePath"] != "/a.go" {
		t.Fatalf("thread context = %v", tc)
	}
	if err := c.SubmitReview(ctx, core.ReviewInput{Owner: "proj", Repo: "web", Number: 11, Event: core.ReviewEventApprove}); !errors.Is(err, core.ErrOperationNotSupported) {
		t.Fatalf("approve: %v", err)
	}
	if err := c.MarkReady(ctx, "proj", "web", 11); err != nil {
		t.Fatal(err)
	}
	if calls["PATCH /contoso/proj/_apis/git/repositories/web/pullrequests/11"]["isDraft"] != false {
		t.Fatalf("calls = %v", calls)
	}
}

func TestCommentEventHasThread(t *testing.T) {
	h := NewWebhookHandler(WebhookOptions{})
	body := `{"id":"n1","eventType":"ms.vss-code.git-pullrequest-comment-event","resource":{
		"comment":{"id":2,"content":"rename","author":{"id":"u1","uniqueName":"dana@contoso.com"},
			"_links":{"threads":{"href":"https://dev.azure.com/contoso/proj/_apis/git/repositories/r1/pullRequests/11/threads/66"}}},
		"pullRequest":{"pullRequestId":11,"status":"active","title":"T","repository":{"id":"r1","name":"web","project":{"name":"proj"}}}}}`
	evs, err := h.Normalize(core.Integration{TeamID: "t", Config: map[string]string{"org": "contoso"}}, []byte(body))
	if err != nil || len(evs) == 0 {
		t.Fatalf("events = %v err = %v", evs, err)
	}
	if evs[0].Source.ThreadRef != "66" || evs[0].Source.Number != 11 {
		t.Fatalf("source = %+v", evs[0].Source)
	}
}
