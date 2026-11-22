package runner

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func startForwarder(t *testing.T, upstream string, token func() string) *mcpForwarder {
	t.Helper()
	f, err := newMCPForwarder(upstream, token, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Close)
	return f
}

func TestMCPForwarderAddsCurrentToken(t *testing.T) {
	var seen atomic.Value
	var gotPath atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.RequestURI())
		w.WriteHeader(http.StatusNoContent)
	}))
	defer up.Close()
	var tok atomic.Value
	tok.Store("ofs_one")
	f := startForwarder(t, up.URL, func() string { return tok.Load().(string) })

	do := func(path, auth string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, f.base+path, strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := do("/mcp/run/run_1?x=1", ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if seen.Load() != "Bearer ofs_one" {
		t.Fatalf("upstream saw %v", seen.Load())
	}
	if gotPath.Load() != "/mcp/run/run_1?x=1" {
		t.Fatalf("upstream path %v", gotPath.Load())
	}
	tok.Store("ofs_two")
	do("/mcp/run/run_1", "")
	if seen.Load() != "Bearer ofs_two" {
		t.Fatalf("after change: upstream saw %v", seen.Load())
	}
	// An incoming header is replaced.
	do("/mcp/run/run_1", "Bearer attacker")
	if seen.Load() != "Bearer ofs_two" {
		t.Fatalf("incoming header: upstream saw %v", seen.Load())
	}
}

func TestMCPForwarderRefusesOtherPaths(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer up.Close()
	f := startForwarder(t, up.URL, func() string { return "ofs_x" })
	for _, p := range []string{"/runner/v1/session/secrets", "/api/v1/me", "/", "/mcp", "/mcp/../runner/v1/x", "/mcp/a/../../api"} {
		resp, err := http.Get(f.base + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d", p, resp.StatusCode)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream got %d calls", calls.Load())
	}
}

func TestMCPForwarderStreams(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "first\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "second\n")
	}))
	defer up.Close()
	defer close(release)
	f := startForwarder(t, up.URL, func() string { return "ofs_x" })
	resp, err := http.Get(f.base + "/mcp/run/run_1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 6)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(resp.Body, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil || string(buf) != "first\n" {
			t.Fatalf("read %q err %v", buf, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk did not arrive before the response ended")
	}
}

func TestMCPForwarderLocalURL(t *testing.T) {
	f := startForwarder(t, "http://control.test", func() string { return "" })
	got, err := f.localURL("https://control.test/mcp/proxy/run_1/srv_1?a=b")
	if err != nil || got != f.base+"/mcp/proxy/run_1/srv_1?a=b" {
		t.Fatalf("got %q err %v", got, err)
	}
	if _, err := f.localURL("https://control.test/api/v1/me"); err == nil {
		t.Fatal("a path outside /mcp/ was accepted")
	}
}

func TestTurnHarnessRewritesSessionTokenServers(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer up.Close()
	cl, err := runnerclient.New(runnerclient.Options{BaseURL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	cl.SetToken("ofs_secret_value")
	r := &runner{cfg: Config{ControlURL: up.URL}, log: quietLog(), client: cl}
	r.start.Harness = runnerproto.Harness{Type: "claude", MCPServers: map[string]runnerproto.MCPServer{
		"factory": {URL: "https://cp.test/mcp/run/run_1", Auth: "session_token"},
		"other":   {URL: "https://other.test/mcp", Auth: ""},
		"local":   {Command: "tool"},
	}}
	r.startMCPForward()
	t.Cleanup(func() { r.mcpForward.Close() })
	if r.mcpForward == nil {
		t.Fatal("no forwarder")
	}

	fake := &fakeCoreHarness{script: func(int, rtcore.HarnessRequest) []rtcore.HarnessEvent {
		return []rtcore.HarnessEvent{{Kind: "done"}}
	}}
	c := &CoreHarness{H: fake}
	req := baseTurnRequest()
	req.Harness = r.turnHarness()
	var cp capture
	if res := c.Turn(context.Background(), req, cp.emit); res.Outcome != runnerproto.OutcomeDone {
		t.Fatalf("result %+v", res)
	}
	got := fake.reqs[0].MCPServers
	if want := r.mcpForward.base + "/mcp/run/run_1"; got["factory"].URL != want {
		t.Errorf("factory URL %q, want %q", got["factory"].URL, want)
	}
	if got["other"].URL != "https://other.test/mcp" || got["local"].Command != "tool" {
		t.Errorf("other servers changed: %+v", got)
	}
	for name, s := range got {
		for _, v := range s.Headers {
			if strings.Contains(v, "ofs_") {
				t.Errorf("%s has the token in a header", name)
			}
		}
	}
	// The start response keeps the original value.
	if s := r.start.Harness.MCPServers["factory"]; s.URL != "https://cp.test/mcp/run/run_1" || s.Auth != "session_token" {
		t.Errorf("start response changed: %+v", s)
	}
}

func TestTurnHarnessWithoutForwarderDropsServers(t *testing.T) {
	r := &runner{log: quietLog()}
	r.start.Harness = runnerproto.Harness{MCPServers: map[string]runnerproto.MCPServer{
		"factory": {URL: "https://cp.test/mcp/run/run_1", Auth: "session_token"},
		"other":   {URL: "https://other.test/mcp"},
	}}
	got := r.turnHarness().MCPServers
	if _, ok := got["factory"]; ok || len(got) != 1 {
		t.Fatalf("servers %+v", got)
	}
	if len(r.start.Harness.MCPServers) != 2 {
		t.Fatal("start response changed")
	}
}
