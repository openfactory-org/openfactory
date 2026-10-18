package engine

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
)

// testFactoryID is the factory of the runs that startTestRun creates. The
// factory does not exist in the store.
const testFactoryID = "fac_test"

// startTestRun starts a run with the agent configuration cfg and no work
// item. Tests use it when the factory, its agents and the dispatch do not
// matter.
func startTestRun(ctx context.Context, e *Engine, teamID string, cfg core.AgentConfig, prompt string, metadata map[string]string) (*core.Run, error) {
	name := cfg.Name
	if name == "" {
		name = "test"
	}
	r := &core.Run{
		TeamID:      teamID,
		FactoryID:   testFactoryID,
		AgentID:     cfg.ID,
		AgentName:   name,
		Name:        name,
		Prompt:      prompt,
		Config:      cloneAgentConfig(cfg),
		Metadata:    metadata,
		TriggeredBy: "api",
	}
	if err := e.createRun(ctx, r, "api"); err != nil {
		return nil, err
	}
	return r, nil
}
