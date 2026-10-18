package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestMCPBridge(t *testing.T) {
	var sawSession, deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			deleted = r.Header.Get("Mcp-Session-Id") == "s1"
			w.WriteHeader(204)
			return
		}
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(body), `"initialize"`):
			w.Header().Set("Mcp-Session-Id", "s1")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-06-18"}}`))
		case strings.Contains(string(body), `"notifications/initialized"`):
			w.WriteHeader(202)
		default:
			sawSession = r.Header.Get("Mcp-Session-Id") == "s1" && r.Header.Get("Mcp-Protocol-Version") == "2025-06-18"
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":2,\"result\":{\"tools\":[]}}\n\n"))
		}
	}))
	defer srv.Close()
	var out bytes.Buffer
	b := &mcpBridge{url: srv.URL, apiKey: "k", client: srv.Client(), out: &out, errLog: io.Discard}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}` + "\n")
	if err := b.run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"protocolVersion":"2025-06-18"`) || !strings.Contains(out.String(), `"tools":[]`) {
		t.Fatalf("output: %s", out.String())
	}
	if !sawSession || !deleted {
		t.Fatalf("session header forwarded=%v deleted=%v", sawSession, deleted)
	}
}

func TestFormatValidationError(t *testing.T) {
	got := formatValidationError("defs", core.ValidationError{File: "agents/x/agent.md", Line: 3, Field: "model", Message: "is required"})
	if got != "defs/agents/x/agent.md:3: model: is required" {
		t.Fatal(got)
	}
}

func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	for _, path := range [][]string{
		{"serve"}, {"worker"}, {"worker", "list"}, {"validate"}, {"schema"}, {"init"}, {"bootstrap"}, {"mcp"}, {"version"},
		{"factory", "list"}, {"factory", "get"}, {"factory", "create"}, {"factory", "apply"}, {"factory", "run"},
		{"run", "list"}, {"run", "get"}, {"run", "logs"}, {"run", "followup"}, {"run", "cancel"},
		{"work-item", "list"}, {"work-item", "get"}, {"work-item", "send"}, {"work-item", "complete"},
		{"inbox", "list"}, {"inbox", "resolve"}, {"secret", "set"}, {"secret", "list"}, {"secret", "delete"},
		{"webhook", "list"}, {"webhook", "create"}, {"webhook", "deliveries"}, {"benchmark", "run"}, {"benchmark", "list"},
		{"mcp-registry", "list"}, {"mcp-registry", "add"}, {"mcp-registry", "connect"},
	} {
		c, _, err := root.Find(path)
		if err != nil || c.Name() != path[len(path)-1] {
			t.Errorf("command %v not found: %v", path, err)
		}
	}
	// Each run belongs to a factory: no command starts a run without one.
	if c, _, err := root.Find([]string{"agent"}); err == nil && c.Name() == "agent" {
		t.Error("the agent command must not exist")
	}
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil || !strings.HasPrefix(out.String(), "openfactory ") {
		t.Fatalf("version: %v %q", err, out.String())
	}
}

// TestBenchmarkRunValidatesRepetitions is a regression test: the CLI rejects
// repetitions outside 1..10 (the API limit) before contacting the server.
func TestBenchmarkRunValidatesRepetitions(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Error(w, `{"error":{"code":"internal","message":"unexpected"}}`, 500)
	}))
	defer srv.Close()
	for _, reps := range []string{"0", "11", "-1"} {
		root := newRootCmd()
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		root.SetArgs([]string{"--server", srv.URL, "--api-key", "of_test", "benchmark", "run", "payments", "suite", "--repetitions", reps})
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "between 1 and 10") {
			t.Errorf("repetitions %s: err = %v", reps, err)
		}
	}
	if hits != 0 {
		t.Fatalf("server contacted %d times", hits)
	}
	for _, n := range []int{1, 10} {
		if err := validateRepetitions(n); err != nil {
			t.Errorf("validateRepetitions(%d) = %v", n, err)
		}
	}
}
