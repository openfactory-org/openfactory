package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// A run (through its session token, the run_agents tool) names the
// environment of another team as an override. The child must not get that
// team's environment variables.
func TestRunAgentsForeignEnvironment(t *testing.T) {
	f := newPlanFixture(t, nil)
	ctx := context.Background()
	foreign := &core.Environment{ID: "env_foreign", TeamID: "team_other", Name: "prod",
		EnvVars: map[string]string{"OTHER_TEAM_TOKEN": "s3cret"}}
	if err := f.Store.CreateEnvironment(ctx, foreign); err != nil {
		t.Fatal(err)
	}
	root := foremanRun(t, f.TestEnv)
	if root.TeamID == foreign.TeamID {
		t.Fatal("test setup: same team")
	}
	envID := foreign.ID
	kids, err := f.Engine.RunAgents(ctx, core.RunAgentsRequest{ParentRunID: root.ID,
		AgentRunConfigs: []core.ChildRunConfig{{AgentIdentityUID: f.Agent(t, "implement").ID, Prompt: "x",
			Overrides: &core.AgentOverrides{EnvironmentID: &envID}}}})
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("RunAgents with the environment of another team: kids %v, err %v; want ErrInvalid", kids, err)
	}
}
