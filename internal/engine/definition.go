package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// FactoryRuntime holds the parts of the active compiled definition that are not
// materialized as Store resources but are needed at run time (repositories,
// machines, factory-wide skills...). It is persisted in the Store's KV space.
type FactoryRuntime struct {
	VersionID          string                      `json:"version_id"`
	Repositories       []core.RepoRef              `json:"repositories"`
	Machines           map[string]core.MachineSpec `json:"machines,omitempty"`
	Skills             []core.Skill                `json:"skills,omitempty"`
	Secrets            []string                    `json:"secrets,omitempty"`
	MCPServers         map[string]core.MCPServer   `json:"mcp_servers,omitempty"`
	CredentialStrategy string                      `json:"credential_strategy,omitempty"`
	SelfImprovement    *core.SelfImprovementConfig `json:"self_improvement,omitempty"`
	Benchmarks         []core.BenchmarkSuite       `json:"benchmarks,omitempty"`
	ScorerDefaults     *core.AgentOverrides        `json:"scorer_defaults,omitempty"`
	CloudProviders     *core.CloudProviders        `json:"cloud_providers,omitempty"`
	// GithubOnboardingNotices is automations.githubOnboardingNotices (nil = true).
	GithubOnboardingNotices *bool `json:"github_onboarding_notices,omitempty"`
	// MaxConcurrentRuns is maxConcurrentRuns of factory.yaml (0 = no limit).
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
	// Approvers is approvers of factory.yaml (empty = every team member).
	Approvers []string `json:"approvers,omitempty"`
	// RestrictFollowupsToStarter is restrictFollowupsToStarter of factory.yaml.
	RestrictFollowupsToStarter bool `json:"restrict_followups_to_starter,omitempty"`
	// RequireLinkedActor is requireLinkedActor of factory.yaml.
	RequireLinkedActor bool `json:"require_linked_actor,omitempty"`
	// AllowAgentMerge is allowAgentMerge of factory.yaml (default false).
	AllowAgentMerge bool `json:"allow_agent_merge,omitempty"`
	// SpendLimit is spendLimit of factory.yaml (nil = no factory limit).
	SpendLimit *core.SpendLimit `json:"spend_limit,omitempty"`
}

func runtimeKey(factoryID string) string { return "factory:" + factoryID + ":runtime" }

// LoadFactoryRuntime reads the runtime blob of a factory's active definition.
// A factory that was never applied yields an empty runtime and no error.
func LoadFactoryRuntime(ctx context.Context, st core.Store, factoryID string) (*FactoryRuntime, error) {
	rt := &FactoryRuntime{}
	if factoryID == "" {
		return rt, nil
	}
	s, err := st.KVGet(ctx, runtimeKey(factoryID))
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return rt, nil
		}
		return nil, err
	}
	if s == "" {
		return rt, nil
	}
	if err := json.Unmarshal([]byte(s), rt); err != nil {
		return nil, fmt.Errorf("decode factory runtime: %w", err)
	}
	return rt, nil
}

func saveFactoryRuntime(ctx context.Context, st core.Store, factoryID string, rt *FactoryRuntime) error {
	b, err := json.Marshal(rt)
	if err != nil {
		return err
	}
	return st.KVSet(ctx, runtimeKey(factoryID), string(b))
}

var aliasSanitizer = regexp.MustCompile(`[^a-z0-9-]+`)

func defaultAlias(name string) string {
	a := aliasSanitizer.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	return strings.Trim(a, "-")
}

// ApplyDefinition compiles a definition tree, runs the preflight against the
// team and stores the tree as a new DefinitionVersion. On success the
// version, the factory's resources, the runtime, the integrations and the
// factory record are written in one transaction, and the version becomes
// active. A failed write rolls back all of them: the factory stays on its old
// version in all tables. When the compiler or the preflight finds errors, the
// invalid version is stored with its errors, the last valid version stays
// active, and the returned error wraps core.ErrInvalid (the version is still
// returned).
func (e *Engine) ApplyDefinition(ctx context.Context, factoryID string, files map[string]string, author, message, commitSHA string) (*core.DefinitionVersion, error) {
	if e.compile == nil {
		return nil, errors.New("engine: no definition compiler configured")
	}
	defer e.lock("factory:" + factoryID)()

	f, err := e.store.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	now := e.now()
	v := &core.DefinitionVersion{
		ID:        core.NewID("defv"),
		FactoryID: factoryID,
		Files:     files,
		CommitSHA: commitSHA,
		Author:    author,
		Message:   message,
		CreatedAt: now,
	}

	chk, resolved := e.checkDefinition(ctx, f, files)
	for _, w := range chk.Warnings {
		e.log.Warn("definition preflight warning", "factory_id", factoryID, "file", w.File, "field", w.Field, "warning", w.Message)
	}
	if !chk.Valid() {
		verrs := chk.Errors
		v.Valid = false
		v.Errors = verrs
		if err := e.store.CreateDefinitionVersion(ctx, v); err != nil {
			return nil, err
		}
		return v, fmt.Errorf("%w: definition has %d validation error(s): %s", core.ErrInvalid, len(verrs), verrs[0].Error())
	}
	def := chk.Definition

	oldWebhooks, err := e.store.ListWebhooks(ctx, factoryID)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	agents, automations, scorers, webhooks := materialize(factoryID, def, oldWebhooks)
	applyResolvedFilters(automations, resolved)
	for i := range webhooks {
		if webhooks[i].CreatedAt.IsZero() {
			webhooks[i].CreatedAt = now
		}
	}
	rt := &FactoryRuntime{
		VersionID:               v.ID,
		Repositories:            def.Repositories,
		Machines:                def.Machines,
		Skills:                  def.Skills,
		Secrets:                 def.Secrets,
		MCPServers:              def.MCPServers,
		CredentialStrategy:      def.CredentialStrategy,
		SelfImprovement:         def.SelfImprovement,
		Benchmarks:              def.Benchmarks,
		ScorerDefaults:          def.ScorerDefaults,
		CloudProviders:          def.CloudProviders,
		GithubOnboardingNotices: func(b bool) *bool { return &b }(def.GithubOnboardingNotices),
		MaxConcurrentRuns:       def.MaxConcurrentRuns,
		RequireLinkedActor:      def.RequireLinkedActor,
		AllowAgentMerge:         def.AllowAgentMerge,
		SpendLimit:              def.SpendLimit,
		Approvers:               def.Approvers,

		RestrictFollowupsToStarter: def.RestrictFollowupsToStarter,
	}

	nf := *f
	if def.Name != "" {
		nf.Name = def.Name
	}
	switch {
	case def.Alias != "":
		nf.Alias = def.Alias
	case nf.Alias == "":
		nf.Alias = defaultAlias(nf.Name)
	}
	nf.Description = def.Description
	nf.ActiveVersionID = v.ID
	nf.UpdatedAt = now
	v.Valid = true
	applied := now
	err = e.Atomic(ctx, func(ctx context.Context) error {
		if err := e.store.CreateDefinitionVersion(ctx, v); err != nil {
			return fmt.Errorf("create definition version: %w", err)
		}
		if err := e.store.ReplaceFactoryResources(ctx, factoryID, agents, automations, scorers, webhooks); err != nil {
			return fmt.Errorf("replace factory resources: %w", err)
		}
		if err := saveFactoryRuntime(ctx, e.store, factoryID, rt); err != nil {
			return fmt.Errorf("save factory runtime: %w", err)
		}
		if def.IntegrationsSet {
			providers := make([]string, 0, len(def.Integrations))
			settings := make(map[string]map[string]string, len(def.Integrations))
			for _, in := range def.Integrations {
				providers = append(providers, in.Type)
				settings[in.Type] = in.Settings
			}
			if err := e.store.SetFactoryIntegrations(ctx, factoryID, providers, settings); err != nil {
				return fmt.Errorf("set factory integrations: %w", err)
			}
		}
		if err := e.store.UpdateFactory(ctx, &nf); err != nil {
			return fmt.Errorf("update factory: %w", err)
		}
		v.AppliedAt = &applied
		if err := e.store.UpdateDefinitionVersion(ctx, v); err != nil {
			return fmt.Errorf("update definition version: %w", err)
		}
		cp := nf
		e.publish(ctx, core.TopicDefinition, &cp)
		return nil
	})
	if err != nil {
		v.AppliedAt = nil
		return nil, err
	}
	*f = nf
	e.log.Info("definition applied", "factory_id", factoryID, "version_id", v.ID, "agents", len(agents), "automations", len(automations))
	return v, nil
}

func (e *Engine) checkAlias(ctx context.Context, f *core.Factory, def *core.CompiledDefinition) []core.ValidationError {
	alias := def.Alias
	if alias == "" {
		return nil
	}
	other, err := e.store.GetFactoryByAlias(ctx, f.TeamID, alias)
	if err != nil || other == nil || other.ID == f.ID {
		return nil
	}
	return []core.ValidationError{{File: "factory.yaml", Field: "alias", Message: fmt.Sprintf("alias %q is already used by factory %q", alias, other.Name)}}
}

// materialize assigns IDs and factory ownership to compiled resources.
// Agents, automations and scorers get stable IDs from their names. A webhook
// gets a random ID at its first apply and keeps its ID and creation time
// while its name stays (old holds the webhooks of the active version).
func materialize(factoryID string, def *core.CompiledDefinition, old []core.Webhook) ([]core.AgentConfig, []core.Automation, []core.Scorer, []core.Webhook) {
	foremanName := ""
	agents := make([]core.AgentConfig, len(def.Agents))
	for i, a := range def.Agents {
		a = cloneAgentConfig(a)
		a.ID = StableID("agt", factoryID, a.Name)
		a.FactoryID = factoryID
		if a.CredentialStrategy == "" {
			a.CredentialStrategy = def.CredentialStrategy
		}
		if a.Type == core.AgentForeman {
			foremanName = a.Name
		}
		agents[i] = a
	}
	automations := make([]core.Automation, len(def.Automations))
	for i, au := range def.Automations {
		au.ID = StableID("aut", factoryID, au.Name)
		au.FactoryID = factoryID
		if au.Agent == "" {
			au.Agent = foremanName
		}
		automations[i] = au
	}
	scorers := make([]core.Scorer, len(def.Scorers))
	for i, s := range def.Scorers {
		key := s.Slug
		if key == "" {
			key = s.Name
		}
		s.ID = StableID("scr", factoryID, key)
		s.FactoryID = factoryID
		scorers[i] = s
	}
	prev := make(map[string]core.Webhook, len(old))
	for _, w := range old {
		prev[w.Name] = w
	}
	webhooks := make([]core.Webhook, len(def.Webhooks))
	for i, w := range def.Webhooks {
		if p, ok := prev[w.Name]; ok && p.ID != "" {
			w.ID, w.CreatedAt = p.ID, p.CreatedAt
		} else {
			w.ID = core.NewID("whk")
		}
		w.FactoryID = factoryID
		webhooks[i] = w
	}
	return agents, automations, scorers, webhooks
}
