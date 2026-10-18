package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

func newMCPCmd(g *globals) *cobra.Command {
	var path string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Bridge the server's Factory MCP to stdio for local coding agents",
		Long: `Run a stdio MCP server that forwards every message to the Factory MCP of an
Open Factory server (streamable HTTP at <server>/mcp), authenticating with
your API key. Use it to give a local coding agent the factory tools
(list_factories, send_work_item, get_work_item, ...). Example MCP client configuration:

  {"mcpServers": {"factory": {"command": "openfactory", "args": ["mcp"],
    "env": {"OF_SERVER": "https://factory.example.com", "OF_API_KEY": "of_..."}}}}`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if g.apiKey == "" {
				return errors.New("no API key: pass --api-key or set OF_API_KEY")
			}
			b := &mcpBridge{
				url:    strings.TrimRight(g.server, "/") + path,
				apiKey: g.apiKey,
				client: http.DefaultClient,
				out:    cmd.OutOrStdout(),
				errLog: cmd.ErrOrStderr(),
			}
			return b.run(cmd.Context(), cmd.InOrStdin())
		},
	}
	cmd.Flags().StringVar(&path, "path", "/mcp", "path of the Factory MCP endpoint on the server")
	return cmd
}

// mcpBridge relays newline-delimited JSON-RPC messages between stdio and a
// streamable HTTP MCP endpoint.
type mcpBridge struct {
	url    string
	apiKey string
	client *http.Client
	out    io.Writer
	errLog io.Writer

	mu       sync.Mutex // guards session and protocol
	session  string
	protocol string
	outMu    sync.Mutex
	wg       sync.WaitGroup
}

type rpcEnvelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

func (b *mcpBridge) run(ctx context.Context, in io.Reader) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		msg := append([]byte(nil), line...)
		var env rpcEnvelope
		_ = json.Unmarshal(msg, &env)
		if env.Method == "initialize" {
			// Initialization establishes the session; finish it before anything else.
			b.forward(ctx, msg, env)
			continue
		}
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			b.forward(ctx, msg, env)
		}()
	}
	b.wg.Wait()
	b.closeSession()
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (b *mcpBridge) forward(ctx context.Context, msg []byte, env rpcEnvelope) {
	if err := b.post(ctx, msg, env); err != nil {
		fmt.Fprintln(b.errLog, "openfactory mcp:", err)
		if len(env.ID) > 0 && env.Method != "" {
			b.write(mustJSON(map[string]any{
				"jsonrpc": "2.0", "id": env.ID,
				"error": map[string]any{"code": -32603, "message": "bridge: " + err.Error()},
			}))
		}
	}
}

func (b *mcpBridge) post(ctx context.Context, msg []byte, env rpcEnvelope) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.url, bytes.NewReader(msg))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	b.mu.Lock()
	if b.session != "" {
		req.Header.Set("Mcp-Session-Id", b.session)
	}
	if b.protocol != "" {
		req.Header.Set("Mcp-Protocol-Version", b.protocol)
	}
	b.mu.Unlock()
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		b.mu.Lock()
		b.session = sid
		b.mu.Unlock()
	}
	if resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("server returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return b.readSSE(resp.Body, env)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) > 0 {
		b.deliver(body, env)
	}
	return nil
}

func (b *mcpBridge) readSSE(r io.Reader, env rpcEnvelope) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var data bytes.Buffer
	flush := func() {
		if data.Len() > 0 {
			b.deliver(append([]byte(nil), data.Bytes()...), env)
			data.Reset()
		}
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return sc.Err()
}

// deliver writes a server message to stdout, recording the negotiated
// protocol version from the initialize result.
func (b *mcpBridge) deliver(msg []byte, req rpcEnvelope) {
	if req.Method == "initialize" {
		var res rpcEnvelope
		if json.Unmarshal(msg, &res) == nil && len(res.Result) > 0 {
			var r struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(res.Result, &r) == nil && r.ProtocolVersion != "" {
				b.mu.Lock()
				b.protocol = r.ProtocolVersion
				b.mu.Unlock()
			}
		}
	}
	var compact bytes.Buffer
	if json.Compact(&compact, msg) == nil {
		msg = compact.Bytes()
	}
	b.write(msg)
}

func (b *mcpBridge) write(msg []byte) {
	b.outMu.Lock()
	defer b.outMu.Unlock()
	_, _ = b.out.Write(append(msg, '\n'))
}

func (b *mcpBridge) closeSession() {
	b.mu.Lock()
	sid := b.session
	b.mu.Unlock()
	if sid == "" {
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodDelete, b.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", sid)
	req.Header.Set("Authorization", "Bearer "+b.apiKey)
	if resp, err := b.client.Do(req); err == nil {
		resp.Body.Close()
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
