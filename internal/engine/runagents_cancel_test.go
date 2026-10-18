package engine

import (
	"context"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// cancelOnGetAgent cancels a run the first time RunAgents reads an agent. That
// is after RunAgents checked that the parent is not terminal and before it
// creates the child: the same order as a human cancel that arrives while a
// run_agents call is in progress.
type cancelOnGetAgent struct {
	core.Store
	fire func()
}

func (s *cancelOnGetAgent) GetAgent(ctx context.Context, id string) (*core.AgentConfig, error) {
	if s.fire != nil {
		f := s.fire
		s.fire = nil
		f()
	}
	return s.Store.GetAgent(ctx, id)
}

func TestCancelDuringRunAgentsLeavesNoLiveChild(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	root := foremanRun(t, env)
	hook := &cancelOnGetAgent{Store: env.Store}
	hook.fire = func() {
		if err := env.Engine.Cancel(ctx, root.ID, "usr_human"); err != nil {
			t.Errorf("cancel: %v", err)
		}
	}
	env.Engine.store = hook
	kids, err := env.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: env.Agent(t, "implement").ID, Prompt: "x"}}})
	env.Engine.store = env.Store
	p, _ := env.Store.GetRun(ctx, root.ID)
	wi, _ := env.Store.GetWorkItem(ctx, root.WorkItemID)
	if err == nil && len(kids) > 0 {
		c, _ := env.Store.GetRun(ctx, kids[0].ID)
		if !c.State.Terminal() {
			t.Fatalf("parent %s, work item %s, but run_agents returned no error and its child %s is %s", p.State, wi.Stage, c.ID, c.State)
		}
	}
}
