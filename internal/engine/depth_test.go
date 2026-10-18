package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// depthEnv returns an environment where implement can start review, so a
// child run is allowed by spawnableBy to start a grandchild.
func depthEnv(t *testing.T) (*TestEnv, *core.Run, *core.Run) {
	t.Helper()
	env := NewTestEnv(t, func(d *core.CompiledDefinition) {
		for i := range d.Agents {
			if d.Agents[i].Name == "review" {
				d.Agents[i].SpawnableBy = []string{"foreman", "implement"}
			}
		}
	})
	root := foremanRun(t, env)
	kids, err := env.Engine.RunAgents(context.Background(), core.RunAgentsRequest{ParentRunID: root.ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "implement").ID, Prompt: "x"}}})
	if err != nil {
		t.Fatalf("root starts a child: %v", err)
	}
	return env, root, kids[0]
}

func TestOrchestrationDepthDefaultStopsChild(t *testing.T) {
	env, _, child := depthEnv(t)
	_, err := env.Engine.RunAgents(context.Background(), core.RunAgentsRequest{ParentRunID: child.ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "review").ID, Prompt: "x"}}})
	if !errors.Is(err, core.ErrForbidden) || !strings.Contains(err.Error(), "OF_MAX_ORCHESTRATION_DEPTH is 1") {
		t.Fatalf("child at the limit: err = %v, want a depth error", err)
	}
	if runs, _ := env.Store.ListRuns(context.Background(), core.RunFilter{ParentRunID: child.ID}); len(runs) != 0 {
		t.Fatalf("grandchildren = %d, want 0", len(runs))
	}
}

func TestOrchestrationDepthTwoAllowsGrandchild(t *testing.T) {
	env, _, child := depthEnv(t)
	env.Engine.maxDepth = 2
	ctx := context.Background()
	grand, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: child.ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "review").ID, Prompt: "x"}}})
	if err != nil {
		t.Fatalf("child at depth 1 with limit 2: %v", err)
	}
	// The grandchild is at depth 2, which is the limit.
	_, err = env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: grand[0].ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "review").ID, Prompt: "x"}}})
	if !errors.Is(err, core.ErrForbidden) || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("grandchild at the limit: err = %v, want a depth error", err)
	}
}

func TestNewEngineDepthDefault(t *testing.T) {
	if e := New(Options{Store: newTestStore(t), Logger: DiscardLogger()}); e.maxDepth != 1 {
		t.Fatalf("maxDepth = %d, want 1", e.maxDepth)
	}
}
