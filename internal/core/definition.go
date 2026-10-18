package core

import "context"

// RepoRef is a repository a factory is scoped to.
type RepoRef struct {
	CodeForge string `json:"codeForge"` // GITHUB, GITLAB, AZURE_DEVOPS
	Owner     string `json:"owner"`
	Name      string `json:"name"`
}

func (r RepoRef) FullName() string { return r.Owner + "/" + r.Name }

type BenchmarkAgentPin struct {
	Agent   string         `json:"agent"`
	Model   string         `json:"model,omitempty"`
	Harness *HarnessConfig `json:"harness,omitempty"`
}

type BenchmarkConfiguration struct {
	Name   string              `json:"name"`
	Agents []BenchmarkAgentPin `json:"agents,omitempty"`
	// Machine is the machine of the trials of this configuration (a machine name).
	Machine string `json:"machine,omitempty"`
}

type BenchmarkTask struct {
	Slug            string   `json:"slug"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	SuccessCriteria string   `json:"successCriteria"`
	SourceRunID     string   `json:"sourceRunId,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	// TrackerSeed is the start state of the tracker for the trial (task
	// trackerSeed, or the older name linearSeed).
	TrackerSeed      any            `json:"trackerSeed,omitempty"`
	StartingRepoRefs []RepoAtCommit `json:"startingRepoRefs,omitempty"`
}

type RepoAtCommit struct {
	CodeForge string `json:"codeForge"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	Ref       string `json:"ref"` // 40-char sha
}

type BenchmarkSuite struct {
	Slug           string                   `json:"slug"`
	Name           string                   `json:"name"`
	Description    string                   `json:"description,omitempty"`
	Agent          string                   `json:"agent"`
	Configurations []BenchmarkConfiguration `json:"configurations,omitempty"`
	Tasks          []BenchmarkTask          `json:"tasks,omitempty"`
}

// RepoSubstitution replaces a factory repository (Source) with another (Target)
// in benchmark starting states.
type RepoSubstitution struct {
	Source RepoRef `json:"source"`
	Target RepoRef `json:"target"`
}

type IntegrationDecl struct {
	Type     string            `json:"type"` // slack, microsoft-teams, linear, jira
	Settings map[string]string `json:"settings,omitempty"`
}

type SelfImprovementConfig struct {
	FailedRunThreshold int      `json:"failedRunThreshold,omitempty"`
	ReviewerType       string   `json:"reviewerType,omitempty"` // admins, team, custom, none
	ReviewerEmails     []string `json:"reviewerEmails,omitempty"`
	// AnalysisModel is the model of Self-improvement runs. Empty means the
	// model of the foreman.
	AnalysisModel string `json:"analysisModel,omitempty"`
}

type CloudProviders struct {
	GCP *struct {
		ProjectNumber                        string `json:"projectNumber"`
		WorkloadIdentityFederationPoolID     string `json:"workloadIdentityFederationPoolId"`
		WorkloadIdentityFederationProviderID string `json:"workloadIdentityFederationProviderId"`
		ServiceAccountEmail                  string `json:"serviceAccountEmail,omitempty"`
	} `json:"gcp,omitempty"`
	AWS *struct {
		RoleARN string `json:"roleArn"`
	} `json:"aws,omitempty"`
	// Audiences are more audiences for which a run can get a cloud
	// identity token, for other services that accept OIDC tokens.
	Audiences []string `json:"audiences,omitempty"`
}

// CompiledDefinition is the validated, resolved form of a definition tree.
// Agent.ID / Automation.ID / FactoryID fields are left empty; the engine assigns stable IDs on apply.
type CompiledDefinition struct {
	SchemaVersion      string            `json:"schemaVersion"`
	Name               string            `json:"name"`
	Description        string            `json:"description,omitempty"`
	Alias              string            `json:"alias,omitempty"`
	CredentialStrategy string            `json:"credentialStrategy,omitempty"`
	CodeForges         []string          `json:"codeForges,omitempty"`
	Repositories       []RepoRef         `json:"repositories"`
	Integrations       []IntegrationDecl `json:"integrations,omitempty"`
	IntegrationsSet    bool              `json:"integrationsSet"` // false => preserve existing
	Secrets            []string          `json:"secrets,omitempty"`
	// MCPServers are the factory-wide MCP servers (factory.yaml mcpServers),
	// granted to every agent and run regardless of overrides.
	MCPServers              map[string]MCPServer   `json:"mcpServers,omitempty"`
	CloudProviders          *CloudProviders        `json:"cloudProviders,omitempty"`
	SelfImprovement         *SelfImprovementConfig `json:"selfImprovement,omitempty"`
	GithubOnboardingNotices bool                   `json:"githubOnboardingNotices"`
	FactoryConfigChecks     string                 `json:"factoryConfigChecks,omitempty"` // main | all
	Agents                  []AgentConfig          `json:"agents"`
	Automations             []Automation           `json:"automations"`
	Machines                map[string]MachineSpec `json:"machines"`
	Scorers                 []Scorer               `json:"scorers"`
	ScorerDefaults          *AgentOverrides        `json:"scorerDefaults,omitempty"`
	Webhooks                []Webhook              `json:"webhooks"`
	Benchmarks              []BenchmarkSuite       `json:"benchmarks"`
	// BenchmarkRepoSubstitutions from factory.yaml.
	BenchmarkRepoSubstitutions []RepoSubstitution `json:"benchmarkRepoSubstitutions,omitempty"`
	Skills                     []Skill            `json:"skills"` // factory-wide skills (agent skills are on AgentConfig)

	// MaxConcurrentRuns limits the open sessions of the factory. 0 means no limit.
	MaxConcurrentRuns int `json:"maxConcurrentRuns,omitempty"`

	// AllowAgentMerge lets agents of the factory call merge tools of MCP
	// servers (allowAgentMerge of factory.yaml). The default is false: the
	// control plane refuses each merge call.
	AllowAgentMerge bool `json:"allowAgentMerge,omitempty"`
	// SpendLimit is spendLimit of factory.yaml. Nil means no factory limit.
	SpendLimit *SpendLimit `json:"spendLimit,omitempty"`

	// Approvers lists the user emails that can resolve an approval. Empty
	// means every team member can.
	Approvers []string `json:"approvers,omitempty"`
	// RestrictFollowupsToStarter limits follow-up messages to the user who
	// started the work item.
	RestrictFollowupsToStarter bool `json:"restrictFollowupsToStarter,omitempty"`
	// RequireLinkedActor drops events from forges, chats and trackers whose
	// sender has no link to a team member.
	RequireLinkedActor bool `json:"requireLinkedActor,omitempty"`
}

// DefinitionCompiler validates and compiles a definition tree (implemented by internal/definition.Compile).
type DefinitionCompiler func(files map[string]string) (*CompiledDefinition, []ValidationError)

// ForgeLookup names one forge action: the team, the factory and one
// repository. The owner of the repository selects the integration record.
type ForgeLookup struct {
	TeamID string
	// FactoryID is the factory of the action. Empty when the action belongs
	// to no factory: then only a team record can serve it.
	FactoryID string
	// CodeForge is GITHUB, GITLAB, AZURE_DEVOPS or GIT.
	CodeForge string
	// Repo is the repository ("owner/name"). Its owner selects the record.
	Repo string
	// Scope optionally limits the credential to these repositories
	// ("owner/name"), where the provider can do that. A run sets it to the
	// repositories of the run.
	Scope []string
}

// ForgeResolver returns an authenticated forge client for one repository of
// a team (implemented in internal/forge). It never uses "the first record":
// when no record serves the owner of the repository, it returns an error
// that names the owner.
type ForgeResolver interface {
	Forge(ctx context.Context, l ForgeLookup) (Forge, error)
}

// DefinitionCheck is the result of the compiler and the preflight for one
// definition tree. The compiler checks the files. The preflight checks the
// files against the team: secrets, environments, worker hosts, registered MCP
// servers, webhooks and name filters.
type DefinitionCheck struct {
	// Definition is the compiled definition (nil when the files do not compile).
	Definition *CompiledDefinition `json:"-"`
	// Errors block an apply.
	Errors []ValidationError `json:"errors"`
	// Warnings do not block an apply. Message holds the reason.
	Warnings []ValidationError `json:"warnings"`
	// Skipped lists the checks that were not possible now, each with a reason.
	Skipped []SkippedCheck `json:"skipped"`
}

// Valid reports whether the definition compiled and the preflight found no errors.
func (c *DefinitionCheck) Valid() bool { return c != nil && c.Definition != nil && len(c.Errors) == 0 }

// SkippedCheck is a preflight check that could not run, and why.
type SkippedCheck struct {
	Check  string `json:"check"`
	Reason string `json:"reason"`
}

// CheckRun is a check run of a forge that supports check runs: a result with
// a title, a summary and a text, shown on a commit or a pull request. The
// definition check of a pull request uses it.
type CheckRun struct {
	Name       string
	HeadSHA    string
	Status     string // in_progress, completed
	Conclusion string // success, failure (when completed)
	Title      string
	Summary    string
	Text       string
	DetailsURL string
}

// NameResolver resolves the names in automation filters (channels, users,
// teams, projects, states, issues) to provider IDs. It is implemented by the
// integrations that can list these objects. scope holds the already resolved
// IDs of other filters of the same trigger, keyed by native filter key (for
// example team_ids for states and issues). The result maps each name to all
// IDs that carry it. An error means that the names cannot be resolved now
// (for example, the integration is not connected); the preflight then
// reports a warning with the error as reason.
type NameResolver interface {
	ResolveNames(ctx context.Context, teamID, provider, kind string, names []string, scope map[string][]string) (map[string][]string, error)
}

// SpendLimit is a limit on model spend, in US dollars. A zero field means no
// limit for that period. Days and months are calendar periods in UTC.
type SpendLimit struct {
	DailyUSD   float64 `json:"daily,omitempty"`
	MonthlyUSD float64 `json:"monthly,omitempty"`
}
