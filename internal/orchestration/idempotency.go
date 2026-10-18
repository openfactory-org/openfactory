package orchestration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TurnSource returns the ID of the active turn of a run. It returns "" when
// the run has no active turn or no turns.
type TurnSource func(ctx context.Context, runID string) (string, error)

// SetTurnSource sets the source of the active turn. With a source, a tool call
// that repeats the same input in the same turn returns the stored result and
// does not run again. Without a source, tools always run.
func (s *Server) SetTurnSource(fn func(ctx context.Context, runID string) (string, error)) {
	s.turn = fn
}

// idempotencyKey is the key of one tool call in the key/value store.
func idempotencyKey(runID, turnID, tool string, input any) (string, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "orch:idem:" + runID + ":" + turnID + ":" + tool + ":" + hex.EncodeToString(sum[:]), nil
}

// withIdempotency wraps a tool handler. In one turn, the same input gives the
// same result and the action runs once. A repeated result carries one more
// text item {"duplicate": true} and the metadata {"duplicate": true}. Results
// with IsError are not stored, so that the model can retry after an error.
func withIdempotency[In any](t *tools, name string, h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		key := t.idempotencyKey(ctx, name, in)
		if key == "" {
			return h(ctx, req, in)
		}
		if stored, err := t.s.store.KVGet(ctx, key); err == nil {
			var texts []string
			if json.Unmarshal([]byte(stored), &texts) == nil {
				return duplicateResult(texts), nil, nil
			}
		} else if !errors.Is(err, core.ErrNotFound) {
			t.s.log.Warn("orchestration: idempotency read", "run", t.runID, "tool", name, "err", err)
		}
		res, out, err := h(ctx, req, in)
		if err != nil || res == nil || res.IsError {
			return res, out, err
		}
		var texts []string
		for _, c := range res.Content {
			tc, ok := c.(*mcp.TextContent)
			if !ok {
				// A result with other content cannot be replayed.
				return res, out, err
			}
			texts = append(texts, tc.Text)
		}
		if raw, merr := json.Marshal(texts); merr == nil {
			if serr := t.s.store.KVSet(ctx, key, string(raw)); serr != nil {
				t.s.log.Warn("orchestration: idempotency write", "run", t.runID, "tool", name, "err", serr)
			}
		}
		return res, out, err
	}
}

// idempotencyKey returns the key for a call, or "" when the call has no
// idempotency (no turn source, no active turn or an error).
func (t *tools) idempotencyKey(ctx context.Context, name string, in any) string {
	if t.s.turn == nil {
		return ""
	}
	turnID, err := t.s.turn(ctx, t.runID)
	if err != nil {
		t.s.log.Warn("orchestration: read active turn", "run", t.runID, "err", err)
		return ""
	}
	if turnID == "" {
		return ""
	}
	key, err := idempotencyKey(t.runID, turnID, name, in)
	if err != nil {
		return ""
	}
	return key
}

func duplicateResult(texts []string) *mcp.CallToolResult {
	res := &mcp.CallToolResult{Meta: mcp.Meta{"duplicate": true}}
	for _, s := range texts {
		res.Content = append(res.Content, &mcp.TextContent{Text: s})
	}
	res.Content = append(res.Content, &mcp.TextContent{Text: `{"duplicate": true}`})
	return res
}
