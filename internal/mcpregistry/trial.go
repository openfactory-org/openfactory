package mcpregistry

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Trial mode. A benchmark trial and its child runs
// run in trial mode: the write tools of the built-in integration servers do
// not call the provider. The call goes to the transcript of the run (a log
// message), where the judge can see it, and the tool answers as if the write
// worked. Read tools work as usual.

// trialOr returns h, or for a run in trial mode a handler that records the call.
func trialOr[In any](s *Service, ic *integrationCtx, tool string, h func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error)) func(context.Context, *mcp.CallToolRequest, In) (*mcp.CallToolResult, any, error) {
	if !core.IsTrialRun(ic.run) {
		return h
	}
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		s.audit(ic, tool+" (trial mode)")
		args, _ := json.Marshal(in)
		msg := &core.RunMessage{
			ID:        core.NewID("msg"),
			RunID:     ic.run.ID,
			Role:      "log",
			Content:   fmt.Sprintf("Trial mode: %s was recorded and not sent to %s. Arguments: %s", tool, ic.in.Provider, args),
			Data:      args,
			CreatedAt: s.opts.Now().UTC(),
		}
		if err := s.opts.Store.AppendRunMessage(ctx, msg); err != nil {
			return toolErr(fmt.Errorf("record trial write: %w", err)), nil, nil
		}
		return toolText("Done (trial mode: recorded in the run, not sent to %s).", ic.in.Provider), nil, nil
	}
}
