package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"

	"github.com/openfactory-org/openfactory/internal/core"
)

// StableID derives a deterministic identifier for a definition resource so that
// re-applying a definition keeps agent/automation identities stable.
func StableID(prefix, factoryID, name string) string {
	sum := sha256.Sum256([]byte(factoryID + "\x00" + prefix + "\x00" + name))
	return prefix + "_" + hex.EncodeToString(sum[:])[:20]
}

// cloneAgentConfig deep-copies the mutable parts of an AgentConfig.
func cloneAgentConfig(c core.AgentConfig) core.AgentConfig {
	out := c
	out.Secrets = slices.Clone(c.Secrets)
	out.SpawnableBy = slices.Clone(c.SpawnableBy)
	if c.SpawnableBy != nil && out.SpawnableBy == nil {
		out.SpawnableBy = []string{}
	}
	out.Skills = slices.Clone(c.Skills)
	if c.MCPServers != nil {
		out.MCPServers = make(map[string]core.MCPServer, len(c.MCPServers))
		for k, v := range c.MCPServers {
			v.Args = slices.Clone(v.Args)
			v.Env = maps.Clone(v.Env)
			v.Headers = maps.Clone(v.Headers)
			out.MCPServers[k] = v
		}
	}
	if c.Machine != nil {
		r := *c.Machine
		r.SetupCommands = slices.Clone(c.Machine.SetupCommands)
		out.Machine = &r
	}
	if c.Harness.Auth != nil {
		a := *c.Harness.Auth
		out.Harness.Auth = &a
	}
	out.NetworkPolicy.AllowedHosts = slices.Clone(c.NetworkPolicy.AllowedHosts)
	out.NetworkPolicy.DeniedHosts = slices.Clone(c.NetworkPolicy.DeniedHosts)
	return out
}

// ApplyOverrides applies sparse execution overrides to an agent configuration.
// machines resolves a machine name to its spec (from the factory definition).
func ApplyOverrides(cfg core.AgentConfig, ov *core.AgentOverrides, machines map[string]core.MachineSpec) (core.AgentConfig, error) {
	out := cloneAgentConfig(cfg)
	if ov == nil {
		return out, nil
	}
	if ov.Harness != nil && ov.Model != "" {
		return out, fmt.Errorf("%w: set at most one of model and harness", core.ErrInvalid)
	}
	if h := ov.Harness; h != nil && h.Type != "" {
		// A harness with a type is complete (an automation's harness is
		// compiled to its full effective value): it replaces the harness of
		// the agent as a whole, so a cleared auth stays cleared.
		out.Harness = *h
		if h.Auth != nil {
			a := *h.Auth
			out.Harness.Auth = &a
		}
	} else if h != nil {
		// A partial harness without a type changes only the fields it sets.
		if h.Model != "" {
			out.Harness.Model = h.Model
		}
		if h.ReasoningLevel != "" {
			out.Harness.ReasoningLevel = h.ReasoningLevel
		}
		if h.Auth != nil {
			a := *h.Auth
			out.Harness.Auth = &a
		}
	}
	if ov.Model != "" {
		out.Harness.Model = ov.Model
	}
	if ov.Machine != "" {
		spec, ok := machines[ov.Machine]
		if !ok {
			return out, fmt.Errorf("%w: unknown machine %q", core.ErrInvalid, ov.Machine)
		}
		spec.SetupCommands = slices.Clone(spec.SetupCommands)
		if spec.Name == "" {
			spec.Name = ov.Machine
		}
		out.Machine = &spec
	}
	if ov.EnvironmentID != nil {
		out.EnvironmentID = *ov.EnvironmentID
	}
	// secrets / mcpServers add to the agent's lists: an override cannot
	// remove a grant by accident. A secret of the override that the agent
	// already lists stays once, in the order of the agent. An MCP server of
	// the override with the name of an agent server replaces that server.
	// Factory-wide entries are added by withFactoryWide.
	for _, name := range ov.Secrets {
		if !slices.Contains(out.Secrets, name) {
			out.Secrets = append(out.Secrets, name)
		}
	}
	if len(ov.MCPServers) > 0 && out.MCPServers == nil {
		out.MCPServers = make(map[string]core.MCPServer, len(ov.MCPServers))
	}
	for k, v := range ov.MCPServers {
		v.Args = slices.Clone(v.Args)
		v.Env = maps.Clone(v.Env)
		v.Headers = maps.Clone(v.Headers)
		out.MCPServers[k] = v
	}
	if ov.WorkerHost != nil {
		out.WorkerHost = *ov.WorkerHost
	}
	if ov.IdleTimeoutMinutes != nil {
		n := *ov.IdleTimeoutMinutes
		if n < 0 || n > 60 {
			return out, fmt.Errorf("%w: idle_timeout_minutes must be between 0 and 60", core.ErrInvalid)
		}
		out.IdleTimeoutMinutes = n
		out.IdleTimeoutSet = true
	}
	return out, nil
}

// teamEnvironment returns environment id when it belongs to team. An
// environment of another team is not found for the caller, as for POST /runs:
// an override (run_agents environment_id or remote.environment_id) cannot
// name it.
func teamEnvironment(ctx context.Context, st core.Store, teamID, id string) (*core.Environment, error) {
	envr, err := st.GetEnvironment(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: environment %s does not exist", core.ErrInvalid, id)
		}
		return nil, err
	}
	if envr.TeamID != teamID {
		return nil, fmt.Errorf("%w: environment %s does not exist", core.ErrInvalid, id)
	}
	return envr, nil
}

// remoteOverrides converts well-known keys of a run_agents "remote" object into
// batch-level overrides (environment_id, worker_host, machine, model).
func remoteOverrides(remote map[string]any) *core.AgentOverrides {
	if len(remote) == 0 {
		return nil
	}
	var ov core.AgentOverrides
	set := false
	str := func(k string) (string, bool) {
		v, ok := remote[k].(string)
		return v, ok && v != ""
	}
	if v, ok := str("environment_id"); ok {
		ov.EnvironmentID = &v
		set = true
	}
	if v, ok := str("worker_host"); ok {
		ov.WorkerHost = &v
		set = true
	}
	if v, ok := str("machine"); ok {
		ov.Machine = v
		set = true
	}
	if v, ok := str("model"); ok {
		ov.Model = v
		set = true
	}
	if !set {
		return nil
	}
	return &ov
}

// mergeOverrides layers b on top of a (b wins).
func mergeOverrides(a, b *core.AgentOverrides) *core.AgentOverrides {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := *a
	if b.Harness != nil {
		out.Harness = b.Harness
		out.Model = ""
	}
	if b.Model != "" {
		out.Model = b.Model
		out.Harness = nil
	}
	if b.Machine != "" {
		out.Machine = b.Machine
	}
	if b.EnvironmentID != nil {
		out.EnvironmentID = b.EnvironmentID
	}
	if b.Secrets != nil {
		out.Secrets = b.Secrets
	}
	if b.MCPServers != nil {
		out.MCPServers = b.MCPServers
	}
	if b.WorkerHost != nil {
		out.WorkerHost = b.WorkerHost
	}
	if b.IdleTimeoutMinutes != nil {
		out.IdleTimeoutMinutes = b.IdleTimeoutMinutes
	}
	return &out
}

// CanSpawn reports whether an agent may be started by the caller agent.
// spawnableBy nil means foreman only; an empty list means nobody; otherwise exact agent names.
// A foreman agent can never be spawned as a child.
func CanSpawn(target core.AgentConfig, callerName string, callerType core.AgentType) bool {
	if target.Type == core.AgentForeman {
		return false
	}
	if target.SpawnableBy == nil {
		return callerType == core.AgentForeman
	}
	return slices.Contains(target.SpawnableBy, callerName)
}

// withFactoryWide adds the factory-wide secrets and MCP servers of the active
// definition to cfg. They apply to every run, whatever the agent declares or
// an override set; an agent/override entry with the same MCP server name
// is kept (the definition rejects such conflicts).
func withFactoryWide(cfg core.AgentConfig, rt *FactoryRuntime) core.AgentConfig {
	if rt == nil {
		return cfg
	}
	for _, s := range rt.Secrets {
		if !slices.Contains(cfg.Secrets, s) {
			cfg.Secrets = append(cfg.Secrets, s)
		}
	}
	for k, v := range rt.MCPServers {
		if _, ok := cfg.MCPServers[k]; ok {
			continue
		}
		if cfg.MCPServers == nil {
			cfg.MCPServers = map[string]core.MCPServer{}
		}
		cfg.MCPServers[k] = v
	}
	return cfg
}
