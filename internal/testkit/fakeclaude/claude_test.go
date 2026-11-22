package fakeclaude

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/harness/claude"
	"github.com/openfactory-org/openfactory/internal/harness/harnesstest"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// standin runs the stand-in in dir with HOME set to home.
func standin(t *testing.T, dir, home, prompt string, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("HOME", home)
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(old) }()
	var out, errb bytes.Buffer
	code := Run(append([]string{"-p", "--output-format", "stream-json", "--verbose", "--input-format", "text"}, args...), strings.NewReader(prompt), &out, &errb)
	return out.String(), errb.String(), code
}

// parse feeds the output to the claude harness and returns its events.
func parse(t *testing.T, stdout string) []rtcore.HarnessEvent {
	t.Helper()
	sb := harnesstest.New("/work")
	sb.StartFunc = func(context.Context, rtcore.ExecRequest) (rtcore.Process, error) {
		return harnesstest.NewProcess(stdout, "", 0), nil
	}
	ch, err := claude.New(claude.Options{}).Run(context.Background(), sb, rtcore.HarnessRequest{
		RunID: "run_1", Harness: rtcore.HarnessConfig{Type: rtcore.HarnessClaude}, Prompt: "x",
	})
	if err != nil {
		t.Fatal(err)
	}
	var evs []rtcore.HarnessEvent
	for ev := range ch {
		evs = append(evs, ev)
	}
	return evs
}

func find(evs []rtcore.HarnessEvent, kind string) []rtcore.HarnessEvent {
	var out []rtcore.HarnessEvent
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestStandinToolsAndResume(t *testing.T) {
	dir, home := t.TempDir(), t.TempDir()
	prompt := `R1 tok. [[tool: write_file {"path":"out/a.txt","content":"hello"}]] [[tool: bash {"command":"cat out/a.txt; echo PUSHED"}]] [[tool: nope {}]]`
	stdout, stderr, code := standin(t, dir, home, prompt, "--model", "claude-sonnet-4-6")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	evs := parse(t, stdout)
	results := find(evs, harness.EventToolResult)
	if len(results) != 3 || !strings.Contains(results[1].Content, "helloPUSHED") || results[2].Content != "NO_TOOL nope" || results[2].Data["is_error"] != true {
		t.Fatalf("tool results = %+v", results)
	}
	done := find(evs, harness.EventDone)
	if len(done) != 1 || done[0].Content != "DONE" || done[0].Usage == nil || done[0].Usage.CostUSD != 0 {
		t.Fatalf("done = %+v", done)
	}
	sess := find(evs, harness.EventSession)
	if len(sess) != 1 || sess[0].SessionID == "" {
		t.Fatalf("session = %+v", sess)
	}
	id := sess[0].SessionID
	// The session file is where the runner looks for it.
	realDir, _ := filepath.EvalSymlinks(dir)
	file := filepath.Join(home, ".claude", "projects", encodeProjectDir(realDir), id+".jsonl")
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("session file: %v", err)
	}

	// A second turn resumes the session and follows a reply directive.
	stdout, stderr, code = standin(t, dir, home, "turn 2 [[reply: TURN2]]", "--resume", id)
	if code != 0 {
		t.Fatalf("resume exit %d: %s", code, stderr)
	}
	evs = parse(t, stdout)
	if d := find(evs, harness.EventDone); len(d) != 1 || d[0].Content != "TURN2" || d[0].SessionID != id {
		t.Fatalf("resume done = %+v", d)
	}
	if h, _ := readSession(file); len(h) != 2 {
		t.Fatalf("session entries = %d", len(h))
	}

	// No directive: READY. An unknown session fails like the real CLI.
	if stdout, _, _ = standin(t, dir, home, "hello"); !strings.Contains(stdout, `"result":"READY"`) {
		t.Fatalf("plain prompt: %s", stdout)
	}
	if _, stderr, code = standin(t, dir, home, "x", "--resume", "missing"); code != 1 || !strings.Contains(stderr, "No conversation found") {
		t.Fatalf("missing session: %d %s", code, stderr)
	}
}

func TestStandinMCPTool(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer t" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		var msg struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		calls = append(calls, msg.Method)
		switch msg.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "s1")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{}})
		case "tools/call":
			if r.Header.Get("Mcp-Session-Id") != "s1" {
				http.Error(w, "no session", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": "posted " + msg.Params.Name + " " + string(msg.Params.Arguments)}},
			}})
			_, _ = w.Write([]byte("event: message\ndata: " + string(b) + "\n\n"))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()
	dir, home := t.TempDir(), t.TempDir()
	cfg := filepath.Join(t.TempDir(), "mcp.json")
	b, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"factory": map[string]any{"type": "http", "url": srv.URL, "headers": map[string]string{"Authorization": "Bearer t"}}}})
	if err := os.WriteFile(cfg, b, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := standin(t, dir, home, `[[tool: mcp__factory__post_update {"text":"hi"}]]`, "--mcp-config", cfg, "--strict-mcp-config")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	res := find(parse(t, stdout), harness.EventToolResult)
	if len(res) != 1 || res[0].Content != `posted post_update {"text":"hi"}` || res[0].Data["is_error"] != false {
		t.Fatalf("result = %+v (calls %v)", res, calls)
	}
}
