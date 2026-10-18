package sdk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/openfactory-org/openfactory/pkg/sdk"
)

func TestInboxAndListForm(t *testing.T) {
	var calls []string
	var bodies []map[string]any
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
		var b map[string]any
		if data, _ := io.ReadAll(r.Body); len(data) > 0 {
			_ = json.Unmarshal(data, &b)
		}
		bodies = append(bodies, b)
		switch r.URL.Path {
		case "/api/v1/inbox":
			if r.URL.Query().Get("cursor") == "" {
				writeJSONBody(w, 200, `{"items":[{"id":"inb_1","kind":"blocked","recipients":["usr_1"],"is_read":false,"questions":[{"question_id":"q1","text":"Which?","type":"single_select","options":["a","b"],"recommended_option":"a"}]}],"page_info":{"has_next_page":true,"next_cursor":"c1"}}`)
				return
			}
			writeJSONBody(w, 200, `{"items":[{"id":"inb_2","kind":"answered","recipients":[]}],"page_info":{"has_next_page":false}}`)
		case "/api/v1/inbox/inb_1/read", "/api/v1/inbox/inb_1/unresolve", "/api/v1/inbox/inb_1/resolve":
			writeJSONBody(w, 200, `{"id":"inb_1","kind":"blocked","is_read":true,"recipients":["usr_1"]}`)
		case "/api/v1/runs":
			writeJSONBody(w, 401, `{"code":"external_authentication_required","status":401,"request_id":"req_1","provider":"github","auth_url":"/api/v1/me/forge-tokens/github","reason":"no_linked_account"}`)
		}
	})
	ctx := context.Background()
	items, err := c.Inbox.List(ctx, &sdk.InboxListOptions{Team: true, Unread: true}).Collect(ctx)
	if err != nil || len(items) != 2 || items[0].Questions[0].RecommendedOption != "a" || items[0].Recipients[0] != "usr_1" {
		t.Fatalf("items = %+v, %v", items, err)
	}
	if calls[0] != "GET /api/v1/inbox?scope=team&unread=true" {
		t.Fatalf("query = %s", calls[0])
	}
	if it, err := c.Inbox.MarkRead(ctx, "inb_1", true); err != nil || !it.IsRead || bodies[len(bodies)-1]["read"] != true {
		t.Fatalf("read = %+v, %v", it, err)
	}
	if _, err := c.Inbox.Unresolve(ctx, "inb_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Inbox.Answer(ctx, "inb_1", []sdk.InboxAnswer{{QuestionID: "q1", Selected: []string{"a"}}}); err != nil {
		t.Fatal(err)
	}
	if a, _ := bodies[len(bodies)-1]["answers"].([]any); len(a) != 1 {
		t.Fatalf("answers body = %v", bodies[len(bodies)-1])
	}
	_, err = c.Runs.List(ctx, &sdk.RunListOptions{Creator: "me", Sources: []string{"API", "CLI"}, Ascending: true}).Collect(ctx)
	var e *sdk.Error
	if !errors.As(err, &e) || e.Code != sdk.CodeExternalAuthRequired || e.Provider != "github" || e.AuthURL == "" || e.RequestID != "req_1" {
		t.Fatalf("err = %#v", err)
	}
	if last := calls[len(calls)-1]; last != "GET /api/v1/runs?creator=me&sort_order=asc&source=API%2CCLI" {
		t.Fatalf("runs query = %s", last)
	}
}
