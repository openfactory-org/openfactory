package mcpregistry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestTrialWritesStayInTheRun shows what a benchmark trial can write to a
// tracker and to chat: the write tools answer, but no request reaches
// the provider, and each call is in the transcript of the run.
func TestTrialWritesStayInTheRun(t *testing.T) {
	f := newFixture(t)
	var writes atomic.Int32
	gql := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if strings.Contains(req.Query, "commentCreate") {
			writes.Add(1)
			io.WriteString(w, `{"data":{"commentCreate":{"success":true}}}`)
			return
		}
		io.WriteString(w, `{"data":{"issue":{"identifier":"ENG-1","title":"Broken login"}}}`)
	}))
	defer gql.Close()
	slackAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "chat.postMessage") {
			writes.Add(1)
		}
		io.WriteString(w, `{"ok":true,"ts":"1.2"}`)
	}))
	defer slackAPI.Close()
	f.svc.opts.LinearGraphQLURL = gql.URL
	f.svc.opts.SlackAPIBaseURL = slackAPI.URL
	_, _ = f.sec.Put(ctx, "team1", "", "LINEAR_TOKEN", core.SecretGeneric, []byte("lin_api_x"))
	_, _ = f.sec.Put(ctx, "team1", "", "SLACK_TOKEN", core.SecretGeneric, []byte("xoxb-x"))
	_ = f.store.UpsertIntegration(ctx, &core.Integration{ID: "int1", TeamID: "team1", Provider: "linear", SecretRef: "LINEAR_TOKEN", Config: map[string]string{}})
	_ = f.store.UpsertIntegration(ctx, &core.Integration{ID: "int2", TeamID: "team1", Provider: "slack", SecretRef: "SLACK_TOKEN", Config: map[string]string{}})
	if err := f.store.CreateFactory(ctx, &core.Factory{ID: "fac1", TeamID: "team1", Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetFactoryIntegrations(ctx, "fac1", []string{"linear", "slack"}, nil); err != nil {
		t.Fatal(err)
	}
	connect := func(run *core.Run, id string) *mcp.ClientSession {
		t.Helper()
		srv, err := f.svc.Resolve(ctx, run, id)
		if err != nil {
			t.Fatal(err)
		}
		hc := &http.Client{Transport: headerRT{h: f.authHeaders(run.ID)}}
		c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
		sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: hc}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		return sess
	}

	trial := &core.Run{ID: core.NewID("run"), TeamID: "team1", FactoryID: "fac1",
		Metadata: map[string]string{core.MetaTrialMode: "true", "benchmark_run": "bench_1"},
		Config: core.AgentConfig{MCPServers: map[string]core.MCPServer{
			"linear": {ManagedID: "builtin/linear", AllowWrite: true},
			"slack":  {ManagedID: "builtin/slack", AllowWrite: true},
		}}}
	if err := f.store.CreateRun(ctx, trial); err != nil {
		t.Fatal(err)
	}
	lin := connect(trial, "builtin/linear")
	res, err := lin.CallTool(ctx, &mcp.CallToolParams{Name: "linear_get_issue", Arguments: map[string]any{"id": "ENG-1"}})
	if err != nil || res.IsError {
		t.Fatalf("read in trial mode: %+v %v", res, err)
	}
	res, err = lin.CallTool(ctx, &mcp.CallToolParams{Name: "linear_create_comment", Arguments: map[string]any{"issue_id": "ENG-1", "body": "Fixed in trial"}})
	if err != nil || res.IsError {
		t.Fatalf("comment in trial mode: %+v %v", res, err)
	}
	sl := connect(trial, "builtin/slack")
	res, err = sl.CallTool(ctx, &mcp.CallToolParams{Name: "slack_post_message", Arguments: map[string]any{"channel": "C1", "text": "Deployed"}})
	if err != nil || res.IsError {
		t.Fatalf("post in trial mode: %+v %v", res, err)
	}
	if n := writes.Load(); n != 0 {
		t.Fatalf("a trial reached the real tracker or chat %d time(s)", n)
	}
	msgs, err := f.store.ListRunMessages(ctx, trial.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range msgs {
		if m.Role == "log" {
			got = append(got, m.Content)
		}
	}
	if len(got) != 2 || !strings.Contains(got[0], "linear_create_comment") || !strings.Contains(got[0], "Fixed in trial") || !strings.Contains(got[1], "slack_post_message") {
		t.Fatalf("recorded writes: %q", got)
	}

	// The same call from a run that is not a trial reaches the provider.
	prod := f.run(map[string]core.MCPServer{"linear": {ManagedID: "builtin/linear", AllowWrite: true}})
	res, err = connect(prod, "builtin/linear").CallTool(ctx, &mcp.CallToolParams{Name: "linear_create_comment", Arguments: map[string]any{"issue_id": "ENG-1", "body": "Real"}})
	if err != nil || res.IsError {
		t.Fatalf("comment: %+v %v", res, err)
	}
	if writes.Load() != 1 {
		t.Fatalf("production write did not reach the tracker")
	}
}
