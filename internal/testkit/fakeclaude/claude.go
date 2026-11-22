// Package fakeclaude is a stand-in for the claude CLI with fixed answers.
// Tests and end-to-end checks run it in place of the real CLI, so that they
// cost no model spend. Main runs it as a program: install a binary named
// claude that calls Main.
package fakeclaude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// The stand-in CLI speaks the stream format of `claude -p --output-format
// stream-json` and returns fixed answers. Tests use it in place of
// the real CLI, so that they cost nothing. It reads directives from the
// prompt on stdin:
//
//	(none)                    reply READY
//	[[reply: TEXT]]           reply TEXT
//	[[tool: NAME {json}]]     call tool NAME with the JSON input. The
//	                          directive may repeat. The tools run in order.
//	                          After the last tool result the reply is DONE.
//
// Tool directives win over a reply directive in the same prompt. The tools:
//
//	bash        {"command": "...", "timeout_seconds": N}  run sh -c in the work directory
//	write_file  {"path": "...", "content": "..."}         write a file
//	read_file   {"path": "..."}                           read a file
//	mcp__S__T   {...}                                     call tool T of the MCP server S
//	                                                      of --mcp-config (HTTP only)
//
// Another tool name gives an error result NO_TOOL NAME. The stand-in keeps a
// session file below ~/.claude/projects, like the real CLI, so that the
// runner saves it in the snapshot and --resume works in a new sandbox.
//
// A tool image can also hold the real CLI as claude-real, next to the
// stand-in. Then a run with a model credential in its environment runs the
// real CLI (PassThrough), and a run without one gets the fixed answers.
const (
	standinReply = "READY"
	standinDone  = "DONE"
)

var directiveRE = regexp.MustCompile(`\[\[(tool|reply):\s*(.*?)\]\]`)

// credentialEnv are the variables that hold a model credential of the real CLI.
var credentialEnv = []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"}

// PassThrough replaces the process with the real CLI when it is installed as
// claude-real next to the stand-in and the environment holds a credential.
// It returns when the stand-in must answer.
func PassThrough(argv []string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	real := filepath.Join(filepath.Dir(exe), "claude-real")
	if _, err := os.Stat(real); err != nil {
		return
	}
	for _, k := range credentialEnv {
		if os.Getenv(k) != "" {
			argv = append([]string{real}, argv[1:]...)
			err := syscall.Exec(real, argv, os.Environ()) //nolint:gosec // test stand-in: runs the real binary next to itself with its own arguments
			fmt.Fprintln(os.Stderr, "claude stand-in: run claude-real:", err)
			os.Exit(1)
		}
	}
}

// standinArgs are the flags of the CLI that the stand-in reads.
type standinArgs struct {
	print     bool
	resume    string
	model     string
	mcpConfig string
	version   bool
}

// flagsWithValue are the flags of the real CLI that take a value.
var flagsWithValue = map[string]bool{
	"--output-format": true, "--input-format": true, "--resume": true, "--model": true, "--effort": true,
	"--append-system-prompt": true, "--append-system-prompt-file": true, "--system-prompt": true,
	"--mcp-config": true, "--permission-mode": true, "--max-turns": true, "--allowedTools": true,
	"--disallowedTools": true, "--add-dir": true, "--settings": true, "--fallback-model": true,
}

func parseStandinArgs(args []string) standinArgs {
	var a standinArgs
	for i := 0; i < len(args); i++ {
		f := args[i]
		val := ""
		if k, v, ok := strings.Cut(f, "="); ok && strings.HasPrefix(f, "--") {
			f, val = k, v
		} else if flagsWithValue[f] && i+1 < len(args) {
			i++
			val = args[i]
		}
		switch f {
		case "-p", "--print":
			a.print = true
		case "-v", "--version":
			a.version = true
		case "--resume", "-r":
			a.resume = val
		case "--model":
			a.model = val
		case "--mcp-config":
			a.mcpConfig = val
		}
	}
	return a
}

// sessionEntry is one line of a session file.
type sessionEntry struct {
	Prompt string `json:"prompt"`
	Reply  string `json:"reply"`
	Time   string `json:"time"`
}

// Run runs the stand-in CLI. It returns the exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	a := parseStandinArgs(args)
	if a.version {
		fmt.Fprintln(stdout, "0.0.0 (Claude Code stand-in)")
		return 0
	}
	if !a.print {
		fmt.Fprintln(stderr, "claude stand-in: only print mode (-p) is supported")
		return 2
	}
	prompt, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "claude stand-in: read prompt:", err)
		return 1
	}
	wd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	sessionDir := filepath.Join(home, ".claude", "projects", encodeProjectDir(wd))
	sessionID := a.resume
	var history []sessionEntry
	if sessionID != "" {
		history, err = readSession(filepath.Join(sessionDir, sessionID+".jsonl"))
		if err != nil {
			fmt.Fprintf(stderr, "No conversation found with session ID: %s\n", sessionID)
			return 1
		}
	} else {
		sessionID = newSessionID()
	}
	model := a.model
	if model == "" {
		model = "claude-stand-in"
	}
	start := time.Now()
	out := json.NewEncoder(stdout)
	emit := func(v any) { _ = out.Encode(v) }
	emit(map[string]any{
		"type": "system", "subtype": "init", "session_id": sessionID, "model": model,
		"tools": []string{"Bash", "Write", "Read"}, "cwd": wd,
	})

	var reply string
	turns := 1
	ctx := context.Background()
	if tools := toolDirectives(string(prompt)); len(tools) > 0 {
		for i, t := range tools {
			id := fmt.Sprintf("toolu_%02d_%s", i+1, sessionID[:8])
			emit(map[string]any{"type": "assistant", "session_id": sessionID, "message": map[string]any{
				"id": "msg_" + id, "role": "assistant", "model": model,
				"content": []map[string]any{{"type": "tool_use", "id": id, "name": t.name, "input": json.RawMessage(t.input)}},
			}})
			text, isErr := callTool(ctx, a.mcpConfig, t.name, t.input)
			emit(map[string]any{"type": "user", "session_id": sessionID, "message": map[string]any{
				"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}},
			}})
			turns++
		}
		reply = standinDone
	} else if r, ok := replyDirective(string(prompt)); ok {
		reply = r
	} else {
		reply = standinReply
	}
	emit(map[string]any{"type": "assistant", "session_id": sessionID, "message": map[string]any{
		"id": "msg_reply_" + sessionID[:8], "role": "assistant", "model": model,
		"content": []map[string]any{{"type": "text", "text": reply}},
	}})

	history = append(history, sessionEntry{Prompt: string(prompt), Reply: reply, Time: time.Now().UTC().Format(time.RFC3339)})
	if err := writeSession(sessionDir, sessionID, history); err != nil {
		fmt.Fprintln(stderr, "claude stand-in: save session:", err)
	}
	emit(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "session_id": sessionID,
		"result": reply, "num_turns": turns, "duration_ms": time.Since(start).Milliseconds(),
		"total_cost_usd": 0, "usage": map[string]any{"input_tokens": len(prompt) / 4, "output_tokens": len(reply) / 4},
	})
	return 0
}

type toolCall struct {
	name  string
	input string
}

// toolDirectives returns the tool directives of a prompt in order.
func toolDirectives(prompt string) []toolCall {
	var out []toolCall
	for _, m := range directiveRE.FindAllStringSubmatch(prompt, -1) {
		if m[1] != "tool" {
			continue
		}
		name, input, _ := strings.Cut(strings.TrimSpace(m[2]), " ")
		input = strings.TrimSpace(input)
		if input == "" {
			input = "{}"
		}
		if !json.Valid([]byte(input)) {
			input = fmt.Sprintf(`{"raw":%q}`, input)
		}
		out = append(out, toolCall{name: name, input: input})
	}
	return out
}

// replyDirective returns the text of the first reply directive.
func replyDirective(prompt string) (string, bool) {
	for _, m := range directiveRE.FindAllStringSubmatch(prompt, -1) {
		if m[1] == "reply" {
			return strings.TrimSpace(m[2]), true
		}
	}
	return "", false
}

// callTool runs one tool. It returns the result text and whether it failed.
func callTool(ctx context.Context, mcpConfig, name, input string) (string, bool) {
	var in struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
		Path           string `json:"path"`
		Content        string `json:"content"`
	}
	_ = json.Unmarshal([]byte(input), &in)
	switch name {
	case "bash":
		timeout := 120 * time.Second
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		}
		cctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(cctx, "sh", "-c", in.Command)
		b, err := cmd.CombinedOutput()
		if err != nil {
			return string(b) + err.Error(), true
		}
		return string(b), false
	case "write_file":
		if in.Path == "" {
			return "path is required", true
		}
		if err := os.MkdirAll(filepath.Dir(in.Path), 0o755); err != nil {
			return err.Error(), true
		}
		if err := os.WriteFile(in.Path, []byte(in.Content), 0o644); err != nil {
			return err.Error(), true
		}
		return fmt.Sprintf("wrote %d bytes to %s", len(in.Content), in.Path), false
	case "read_file":
		b, err := os.ReadFile(in.Path)
		if err != nil {
			return err.Error(), true
		}
		return string(b), false
	}
	if server, tool, ok := strings.Cut(strings.TrimPrefix(name, "mcp__"), "__"); ok && strings.HasPrefix(name, "mcp__") {
		text, err := callMCP(ctx, mcpConfig, server, tool, json.RawMessage(input))
		if err != nil {
			return err.Error(), true
		}
		return text, false
	}
	return "NO_TOOL " + name, true
}

// callMCP calls one tool of an HTTP MCP server of the --mcp-config file.
func callMCP(ctx context.Context, configFile, server, tool string, args json.RawMessage) (string, error) {
	if configFile == "" {
		return "", errors.New("no --mcp-config")
	}
	raw, err := os.ReadFile(configFile)
	if err != nil {
		return "", err
	}
	var cfg struct {
		MCPServers map[string]struct {
			Type    string            `json:"type"`
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", err
	}
	s, ok := cfg.MCPServers[server]
	if !ok || s.URL == "" {
		return "", fmt.Errorf("no HTTP MCP server %q", server)
	}
	c := &mcpClient{url: s.URL, headers: s.Headers, hc: &http.Client{Timeout: 60 * time.Second}}
	if _, err := c.call(ctx, 1, "initialize", map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "claude-stand-in", "version": "0.0.0"},
	}); err != nil {
		return "", fmt.Errorf("initialize: %w", err)
	}
	_, _ = c.call(ctx, 0, "notifications/initialized", nil)
	res, err := c.call(ctx, 2, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return string(res), nil //nolint:nilerr // a result that is not a content list is returned as raw text
	}
	var texts []string
	for _, c := range out.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	if out.IsError {
		return "", errors.New(strings.Join(texts, "\n"))
	}
	return strings.Join(texts, "\n"), nil
}

type mcpClient struct {
	url     string
	headers map[string]string
	session string
	hc      *http.Client
}

// call sends one JSON-RPC message. id 0 sends a notification.
func (c *mcpClient) call(ctx context.Context, id int, method string, params any) (json.RawMessage, error) {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != 0 {
		msg["id"] = id
	}
	if params != nil {
		msg["params"] = params
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if id == 0 {
		return nil, nil
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data = lastSSEData(data)
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &rpc); err != nil {
		return nil, fmt.Errorf("decode answer: %w", err)
	}
	if rpc.Error != nil {
		return nil, errors.New(rpc.Error.Message)
	}
	return rpc.Result, nil
}

// lastSSEData returns the data of the last event of a stream.
func lastSSEData(b []byte) []byte {
	var last []byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		if d, ok := bytes.CutPrefix(sc.Bytes(), []byte("data:")); ok {
			last = append([]byte(nil), bytes.TrimSpace(d)...)
		}
	}
	return last
}

// encodeProjectDir is the directory name of the real CLI for a work directory.
func encodeProjectDir(dir string) string {
	var b strings.Builder
	for _, r := range dir {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func readSession(path string) ([]sessionEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []sessionEntry
	for _, line := range bytes.Split(b, []byte("\n")) {
		var e sessionEntry
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func writeSession(dir, id string, entries []sessionEntry) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	var b bytes.Buffer
	for _, e := range entries {
		line, _ := json.Marshal(e)
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(filepath.Join(dir, id+".jsonl"), b.Bytes(), 0o600)
}

// Main runs the stand-in as a program and exits: the real CLI when
// PassThrough finds it, else the fixed answers.
func Main() {
	PassThrough(os.Args)
	os.Exit(Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
