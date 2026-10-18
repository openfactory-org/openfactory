package mcpregistry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestMergeRefused checks the rule that agents never merge: a merge
// tool call on the proxy or a built-in server gets a refusal, unless the
// factory sets allowAgentMerge, and each merge call is recorded.
func TestMergeRefused(t *testing.T) {
	f := newFixture(t)
	var allow atomic.Bool
	var records []string
	f.svc.opts.AllowAgentMerge = func(_ context.Context, run *core.Run) bool { return run.FactoryID == "fac1" && allow.Load() }
	f.svc.opts.OnMergeCall = func(_ context.Context, run *core.Run, server, tool string, allowed bool) {
		records = append(records, run.ID+"|"+tool+"|"+map[bool]string{true: "allowed", false: "refused"}[allowed])
	}

	// An upstream forge MCP server with a merge tool.
	var merged atomic.Int32
	up := mcp.NewServer(&mcp.Implementation{Name: "forge", Version: "1"}, nil)
	type prInput struct {
		Number int `json:"number"`
	}
	mcp.AddTool(up, &mcp.Tool{Name: "merge_pull_request"}, func(context.Context, *mcp.CallToolRequest, prInput) (*mcp.CallToolResult, any, error) {
		merged.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "merged"}}}, nil, nil
	})
	mcp.AddTool(up, &mcp.Tool{Name: "get_pull_request"}, func(context.Context, *mcp.CallToolRequest, prInput) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "open"}}}, nil, nil
	})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return up }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer upstream.Close()
	m := f.register(&core.RegisteredMCPServer{ID: "mcp_forge", Name: "forge", URL: upstream.URL, Connection: core.MCPConnectionProxy})

	connect := func(run *core.Run, managedID string) *mcp.ClientSession {
		t.Helper()
		srv, err := f.svc.Resolve(ctx, run, managedID)
		if err != nil {
			t.Fatal(err)
		}
		c := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil)
		sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL, HTTPClient: &http.Client{Transport: headerRT{h: f.authHeaders(run.ID)}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { sess.Close() })
		return sess
	}
	run := f.run(map[string]core.MCPServer{"forge": {ManagedID: m.Name}})
	sess := connect(run, m.Name)

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "get_pull_request", Arguments: map[string]any{"number": 1}})
	if err != nil || res.IsError {
		t.Fatalf("read tool: %+v %v", res, err)
	}
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "merge_pull_request", Arguments: map[string]any{"number": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "cannot merge") {
		t.Fatalf("merge must be refused: %+v", res)
	}
	if merged.Load() != 0 {
		t.Fatal("the merge reached the upstream server")
	}

	// The built-in integration servers refuse a merge call before the server.
	_, _ = f.sec.Put(ctx, "team1", "", "LINEAR_TOKEN", core.SecretGeneric, []byte("lin_api_x"))
	_ = f.store.UpsertIntegration(ctx, &core.Integration{ID: "int1", TeamID: "team1", Provider: "linear", SecretRef: "LINEAR_TOKEN", Config: map[string]string{}})
	_ = f.store.CreateFactory(ctx, &core.Factory{ID: "fac1", TeamID: "team1", Name: "web"})
	_ = f.store.SetFactoryIntegrations(ctx, "fac1", []string{"linear"}, nil)
	brun := f.run(map[string]core.MCPServer{"linear": {ManagedID: "builtin/linear", AllowWrite: true}})
	bsess := connect(brun, "builtin/linear")
	res, err = bsess.CallTool(ctx, &mcp.CallToolParams{Name: "merge_pull_request", Arguments: map[string]any{"number": 1}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "cannot merge") {
		t.Fatalf("built-in merge must be refused with the platform text: %+v", res)
	}

	// allowAgentMerge: the call passes and is recorded.
	allow.Store(true)
	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "merge_pull_request", Arguments: map[string]any{"number": 1}})
	if err != nil || res.IsError || merged.Load() != 1 {
		t.Fatalf("allowed merge: %+v %v (merged %d)", res, err, merged.Load())
	}
	want := []string{run.ID + "|merge_pull_request|refused", brun.ID + "|merge_pull_request|refused", run.ID + "|merge_pull_request|allowed"}
	if strings.Join(records, ",") != strings.Join(want, ",") {
		t.Fatalf("records = %v, want %v", records, want)
	}
}
