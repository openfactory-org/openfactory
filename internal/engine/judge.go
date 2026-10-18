package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// StartJudgeRun starts the judge of a scorer as a run in a sandbox. The
// run belongs to the factory but to no work item, so it does not show in the
// work-item metrics. It uses the harness, the machine, the secrets and the MCP
// servers of the request, plus the factory-wide secrets and MCP servers. The
// files of the request are a skill named core.JudgeSkillName in the workspace.
// The repositories are checked out at the start commits of the scored run.
// The session ends after the first turn.
func (e *Engine) StartJudgeRun(ctx context.Context, req core.JudgeRunRequest) (*core.Run, error) {
	if req.FactoryID == "" {
		return nil, fmt.Errorf("%w: factory id is required", core.ErrInvalid)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: prompt is required", core.ErrInvalid)
	}
	if req.Harness.Type == "" || req.Harness.Model == "" {
		return nil, fmt.Errorf("%w: the judge needs a harness with a type and a model (set harness or model on the scorer, scorerDefaults or agentDefaults)", core.ErrInvalid)
	}
	if err := ValidateMetadata(req.Metadata); err != nil {
		return nil, err
	}
	f, err := e.store.GetFactory(ctx, req.FactoryID)
	if err != nil {
		return nil, err
	}
	rt, err := LoadFactoryRuntime(ctx, e.store, f.ID)
	if err != nil {
		return nil, err
	}
	name := req.Name
	if name == "" {
		name = "scorer"
	}
	cfg := core.AgentConfig{
		ID:                 StableID("judge", f.ID, name),
		FactoryID:          f.ID,
		Name:               name,
		Description:        "The judge of a scorer.",
		Type:               core.AgentCustom,
		Instructions:       req.Instructions,
		Harness:            req.Harness,
		Secrets:            append([]string(nil), req.Secrets...),
		IdleTimeoutMinutes: 0,
		IdleTimeoutSet:     true,
		SpawnableBy:        []string{},
	}
	if cfg.Harness.Auth != nil {
		a := *cfg.Harness.Auth
		cfg.Harness.Auth = &a
	}
	if len(req.MCPServers) > 0 {
		cfg.MCPServers = make(map[string]core.MCPServer, len(req.MCPServers))
		for k, v := range req.MCPServers {
			cfg.MCPServers[k] = v
		}
	}
	// The judge works in the same place as the agents: it takes the
	// network policy, the environment and the machine of the foreman unless
	// the scorer names a machine.
	if fm, err := e.foremanOf(ctx, f.ID); err == nil {
		cfg.NetworkPolicy = fm.NetworkPolicy
		cfg.EnvironmentID = fm.EnvironmentID
		cfg.WorkerHost = fm.WorkerHost
		if fm.Machine != nil {
			r := *fm.Machine
			cfg.Machine = &r
		}
		cfg.CredentialStrategy = fm.CredentialStrategy
	}
	if req.Machine != "" {
		spec, ok := rt.Machines[req.Machine]
		if !ok {
			return nil, fmt.Errorf("%w: machine %q is not defined in the factory", core.ErrInvalid, req.Machine)
		}
		if spec.Name == "" {
			spec.Name = req.Machine
		}
		cfg.Machine = &spec
	}
	if len(req.Files) > 0 {
		files := make(map[string]string, len(req.Files))
		for k, v := range req.Files {
			files[k] = v
		}
		cfg.Skills = []core.Skill{{
			Name:        core.JudgeSkillName,
			Description: "The evidence of the run that you score.",
			Path:        "skills/" + core.JudgeSkillName + "/SKILL.md",
			Content:     judgeSkillContent(req.Files),
			Files:       files,
			Scope:       name,
		}}
	}
	cfg = withFactoryWide(cfg, rt)
	triggeredBy := req.TriggeredBy
	if triggeredBy == "" {
		triggeredBy = "scorer"
	}
	run := &core.Run{
		ID:          core.NewID("run"),
		TeamID:      f.TeamID,
		FactoryID:   f.ID,
		AgentID:     cfg.ID,
		AgentName:   name,
		Name:        name,
		Prompt:      req.Prompt,
		Config:      cfg,
		Metadata:    req.Metadata,
		TriggeredBy: triggeredBy,
	}
	if len(req.StartingRepoRefs) > 0 {
		if err := kvPutJSON(ctx, e.store, core.RunRepoRefsKey(run.ID), req.StartingRepoRefs); err != nil {
			return nil, fmt.Errorf("save starting repo refs: %w", err)
		}
	}
	if err := e.createRun(ctx, run, triggeredBy); err != nil {
		return nil, err
	}
	e.log.Info("started judge run", "factory_id", f.ID, "run_id", run.ID, "name", name, "triggered_by", triggeredBy)
	return run, nil
}

func judgeSkillContent(files map[string]string) string {
	var b strings.Builder
	b.WriteString("---\nname: " + core.JudgeSkillName + "\ndescription: The evidence of the run that you score.\n---\n\n")
	b.WriteString("This skill holds the evidence of the run that you score. Read the files before you decide.\n\n")
	for p := range files {
		fmt.Fprintf(&b, "- `%s`\n", p)
	}
	return b.String()
}
