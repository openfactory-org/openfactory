package orchestration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestPostedTextIsRedacted checks that the text of post_update, ask_human and
// request_approval loses the secret values of the run before the engine gets it.
func TestPostedTextIsRedacted(t *testing.T) {
	st := &fakeStore{
		runs: map[string]*core.Run{
			"run_root": {ID: "run_root", FactoryID: "fac", WorkItemID: "wi_1", State: core.RunRunning},
		},
		wi: &core.WorkItem{ID: "wi_1", FactoryID: "fac", Stage: core.StageTriage},
	}
	eng := &fakeEngine{}
	srv := newServer(eng, st)
	var asked string
	srv.SetRedactor(func(_ context.Context, run *core.Run) func(string) string {
		asked = run.ID
		return func(s string) string { return strings.ReplaceAll(s, "sekret-value-1", "***") }
	})
	mux := http.NewServeMux()
	srv.Register(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	sess := connect(t, ts, "run_root")

	callText(t, sess, "post_update", map[string]any{"text": "the key is sekret-value-1"})
	if c := eng.last(); c.Method != "PostUpdate" || c.Args[1] != "the key is ***" {
		t.Fatalf("post_update call = %+v", c)
	}
	callText(t, sess, "ask_human", map[string]any{"question": "Use sekret-value-1?", "context": "sekret-value-1 here", "options": []string{"yes sekret-value-1", "no"}})
	c := eng.last()
	if c.Method != "RequestHuman" || strings.Contains(c.Args[3].(string), "sekret") || strings.Contains(c.Args[2].(string), "sekret") {
		t.Fatalf("ask_human call = %+v", c)
	}
	callText(t, sess, "request_approval", map[string]any{"title": "Spec sekret-value-1", "summary": "sekret-value-1", "links": []string{"https://x/?k=sekret-value-1"}})
	c = eng.last()
	if strings.Contains(c.Args[2].(string), "sekret") || strings.Contains(c.Args[3].(string), "sekret") {
		t.Fatalf("request_approval call = %+v", c)
	}
	if asked != "run_root" {
		t.Errorf("redactor asked for run %q", asked)
	}
}

func TestNoRedactorChangesNothing(t *testing.T) {
	eng, _, ts := newFixture(t)
	callText(t, connect(t, ts, "run_root"), "post_update", map[string]any{"text": "plain text"})
	if c := eng.last(); c.Args[1] != "plain text" {
		t.Fatalf("call = %+v", c)
	}
}
