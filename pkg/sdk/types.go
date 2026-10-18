package sdk

import (
	"encoding/json"
	"time"
)

// Run states reported by the API.
const (
	StateQueued    = "QUEUED"
	StatePending   = "PENDING"
	StateClaimed   = "CLAIMED"
	StateRunning   = "RUNNING"
	StateBlocked   = "BLOCKED"
	StateSucceeded = "SUCCEEDED"
	StateFailed    = "FAILED"
	StateError     = "ERROR"
	StateCancelled = "CANCELLED"
)

// Finished reports whether an API run state is final: SUCCEEDED, FAILED,
// ERROR or CANCELLED.
func Finished(state string) bool {
	switch state {
	case StateSucceeded, StateFailed, StateError, StateCancelled:
		return true
	}
	return false
}

// WaitsForInput reports whether the state is BLOCKED. A blocked run waits
// for a person.
func WaitsForInput(state string) bool { return state == StateBlocked }

// Terminal is the same as Finished.
func Terminal(state string) bool { return Finished(state) }

// RepoRef identifies a repository.
type RepoRef struct {
	CodeForge string `json:"codeForge,omitempty"`
	Owner     string `json:"owner"`
	Name      string `json:"name"`
}

// IntegrationDecl attaches an integration provider to a factory.
type IntegrationDecl struct {
	Type     string            `json:"type"`
	Settings map[string]string `json:"settings,omitempty"`
}

// HarnessAuth configures third-party harness authentication.
type HarnessAuth struct {
	Source     string `json:"source"`
	SecretName string `json:"secretName,omitempty"`
}

// HarnessConfig selects the runtime and model of an agent.
type HarnessConfig struct {
	Type           string       `json:"type"`
	Model          string       `json:"model,omitempty"`
	ReasoningLevel string       `json:"reasoningLevel,omitempty"`
	Auth           *HarnessAuth `json:"auth,omitempty"`
}

// MCPServer is one MCP server transport (exactly one of ManagedID, Command, URL).
type MCPServer struct {
	ManagedID string            `json:"managedId,omitempty"`
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	// AllowWrite enables the write tools of a built-in integration server
	// (ManagedID "builtin/<provider>").
	AllowWrite bool `json:"allowWrite,omitempty"`
}

// RegisteredMCPServer is an entry of the team MCP server registry. Agents
// reference it with MCPServer.ManagedID set to its name or id.
type RegisteredMCPServer struct {
	ID          string            `json:"id"`
	TeamID      string            `json:"team_id"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// Connection is "proxy" (the default for a url server) or "direct".
	// Empty for a command server.
	Connection string          `json:"connection,omitempty"`
	OAuth      *MCPOAuthConfig `json:"oauth,omitempty"`
	CreatedBy  string          `json:"created_by,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
	// Warnings are set on a create or an update response, for example when
	// a legacy SSE server is registered with connection direct.
	Warnings []string `json:"warnings,omitempty"`
}

// MCPOAuthConfig is the OAuth configuration and connection state of a
// registered server. Status is not_connected, connected or error.
type MCPOAuthConfig struct {
	Scopes                []string   `json:"scopes,omitempty"`
	ClientID              string     `json:"client_id,omitempty"`
	ClientSecretName      string     `json:"client_secret_name,omitempty"`
	AuthorizationEndpoint string     `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string     `json:"token_endpoint,omitempty"`
	Status                string     `json:"status,omitempty"`
	ConnectedBy           string     `json:"connected_by,omitempty"`
	ConnectedAt           *time.Time `json:"connected_at,omitempty"`
	Error                 string     `json:"error,omitempty"`
}

// BuiltinMCPServer is a server the control plane serves for an integration.
type BuiltinMCPServer struct {
	ManagedID  string `json:"managed_id"`
	Provider   string `json:"provider"`
	Configured bool   `json:"configured"`
}

// SourceRef identifies where a work item came from.
type SourceRef struct {
	Provider   string `json:"provider,omitempty"`
	Kind       string `json:"kind,omitempty"`
	URL        string `json:"url,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Number     int    `json:"number,omitempty"`
	Channel    string `json:"channel,omitempty"`
	ThreadID   string `json:"thread_id,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Title      string `json:"title,omitempty"`
}

// Factory is a team of agents bound to repositories.
type Factory struct {
	UID             string    `json:"uid"`
	TeamID          string    `json:"team_id"`
	Name            string    `json:"name"`
	Alias           string    `json:"alias"`
	Description     string    `json:"description,omitempty"`
	Source          string    `json:"source"`
	GitRepo         string    `json:"git_repo,omitempty"`
	GitBranch       string    `json:"git_branch,omitempty"`
	GitPath         string    `json:"git_path,omitempty"`
	ActiveVersionID string    `json:"active_version_id,omitempty"`
	URL             string    `json:"url,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ValidationError is one definition validation failure.
type ValidationError struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

// DefinitionVersion is an immutable snapshot of a definition tree.
type DefinitionVersion struct {
	ID        string            `json:"id"`
	FactoryID string            `json:"factory_id"`
	Files     map[string]string `json:"files,omitempty"`
	CommitSHA string            `json:"commit_sha,omitempty"`
	Author    string            `json:"author,omitempty"`
	Message   string            `json:"message,omitempty"`
	Valid     bool              `json:"valid"`
	Errors    []ValidationError `json:"errors,omitempty"`
	AppliedAt *time.Time        `json:"applied_at,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// Definition is a factory's active definition.
type Definition struct {
	FactoryID string             `json:"factory_id"`
	Source    string             `json:"source"`
	Files     map[string]string  `json:"files"`
	Version   *DefinitionVersion `json:"version"`
}

// SkippedCheck is a preflight check that was not possible, and why.
type SkippedCheck struct {
	Check  string `json:"check"`
	Reason string `json:"reason"`
}

// ValidationResult is the outcome of validating a definition tree.
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors"`
	// Warnings come from the preflight and do not block an apply.
	Warnings []ValidationError `json:"warnings,omitempty"`
	// Skipped lists the preflight checks that were not possible, with the reason.
	Skipped []SkippedCheck `json:"skipped,omitempty"`
	Summary *struct {
		Name        string   `json:"name"`
		Alias       string   `json:"alias,omitempty"`
		Agents      []string `json:"agents"`
		Automations []string `json:"automations"`
		Machines    []string `json:"machines"`
		Webhooks    []string `json:"webhooks"`
		Scorers     []string `json:"scorers"`
		Benchmarks  []string `json:"benchmarks"`
	} `json:"summary,omitempty"`
}

// Agent is the resolved configuration of a factory agent.
type Agent struct {
	ID                 string               `json:"id"`
	FactoryID          string               `json:"factory_id"`
	Name               string               `json:"name"`
	Description        string               `json:"description"`
	Type               string               `json:"type"`
	Instructions       string               `json:"instructions"`
	Harness            HarnessConfig        `json:"harness"`
	EnvironmentID      string               `json:"environment_id,omitempty"`
	WorkerHost         string               `json:"worker_host,omitempty"`
	Secrets            []string             `json:"secrets,omitempty"`
	MCPServers         map[string]MCPServer `json:"mcp_servers,omitempty"`
	SpawnableBy        []string             `json:"spawnable_by"`
	IdleTimeoutMinutes int                  `json:"idle_timeout_minutes,omitempty"`
}

// Automation routes triggers to an agent.
type Automation struct {
	ID          string            `json:"id"`
	FactoryID   string            `json:"factory_id"`
	Name        string            `json:"name"`
	DisplayName string            `json:"display_name,omitempty"`
	Enabled     bool              `json:"enabled"`
	Agent       string            `json:"agent"`
	Prompt      string            `json:"prompt"`
	Triggers    []json.RawMessage `json:"triggers"`
}

// Usage is token/cost accounting of a run. Coverage is known, partial or
// unavailable.
type Usage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	Coverage     string  `json:"coverage,omitempty"`
}

// StatusMessage is the status of a run for people.
type StatusMessage struct {
	Message     string `json:"message"`
	FailureCode string `json:"failure_code,omitempty"`
	Retryable   bool   `json:"retryable"`
	Detail      string `json:"detail,omitempty"`
}

// Run is one agent execution.
type Run struct {
	RunID           string            `json:"run_id"`
	State           string            `json:"state"` // QUEUED, PENDING, CLAIMED, RUNNING, BLOCKED, SUCCEEDED, FAILED, ERROR, CANCELLED
	Phase           string            `json:"phase"`
	Title           string            `json:"title"`
	RunURL          string            `json:"run_url"`
	StatusMessage   *StatusMessage    `json:"status_message,omitempty"`
	TeamID          string            `json:"team_id"`
	FactoryID       string            `json:"factory_id,omitempty"`
	WorkItemID      string            `json:"work_item_id,omitempty"`
	ParentRunID     string            `json:"parent_run_id,omitempty"`
	RootRunID       string            `json:"root_run_id,omitempty"`
	Depth           int               `json:"depth,omitempty"`
	ActiveChildRuns int               `json:"active_child_runs,omitempty"`
	Session         int               `json:"session,omitempty"`
	URL             string            `json:"url,omitempty"`
	BlockedReason   string            `json:"blocked_reason,omitempty"`
	FailureCode     string            `json:"failure_code,omitempty"`
	SandboxRunning  bool              `json:"sandbox_running,omitempty"`
	AgentID         string            `json:"agent_id,omitempty"`
	AgentName       string            `json:"agent_name,omitempty"`
	Name            string            `json:"name,omitempty"`
	Prompt          string            `json:"prompt"`
	Config          json.RawMessage   `json:"config,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	Result          string            `json:"result,omitempty"`
	Error           string            `json:"error,omitempty"`
	Usage           Usage             `json:"usage"`
	// Source is how the run started: API, MCP, DASHBOARD, CLI, AUTOMATION,
	// SCHEDULE, WEBHOOK, MENTION, ORCHESTRATION, SCORER, BENCHMARK or
	// SELF_IMPROVEMENT.
	Source string `json:"source"`
	// SourceRef is the ID of the automation, schedule, webhook, parent
	// run, scorer or benchmark.
	SourceRef   string     `json:"source_ref,omitempty"`
	Creator     *Principal `json:"creator,omitempty"`
	RunAs       *Principal `json:"run_as,omitempty"`
	WorkItemURL string     `json:"work_item_url,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Done reports whether the run reached a final state.
func (r *Run) Done() bool { return Finished(r.State) }

// Finished reports whether the run reached a final state.
func (r *Run) Finished() bool { return Finished(r.State) }

// WaitsForInput reports whether the run is BLOCKED and waits for a person.
func (r *Run) WaitsForInput() bool { return WaitsForInput(r.State) }

// RunMessage is one transcript entry.
type RunMessage struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"`
	Role      string          `json:"role"`
	Content   string          `json:"content"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// PullRequestRef is a pull request linked to a work item.
type PullRequestRef struct {
	Forge  string `json:"forge"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	State  string `json:"state"`
}

// WorkItem is one request moving through a factory.
type WorkItem struct {
	ID           string            `json:"id"`
	FactoryID    string            `json:"factory_id"`
	Title        string            `json:"title"`
	Stage        string            `json:"stage"`
	Source       SourceRef         `json:"source"`
	CreatorID    string            `json:"creator_id,omitempty"`
	RootRunID    string            `json:"root_run_id,omitempty"`
	PullRequests []PullRequestRef  `json:"pull_requests,omitempty"`
	Labels       map[string]string `json:"labels,omitempty"`
	LocalOwner   string            `json:"local_owner,omitempty"`
	URL          string            `json:"url"`
	CreatedAt    time.Time         `json:"created_at"`
	UpdatedAt    time.Time         `json:"updated_at"`
	CompletedAt  *time.Time        `json:"completed_at,omitempty"`
}

// WorkItemEvent is one timeline entry.
type WorkItemEvent struct {
	ID         string          `json:"id"`
	WorkItemID string          `json:"work_item_id"`
	Kind       string          `json:"kind"`
	RunID      string          `json:"run_id,omitempty"`
	Actor      string          `json:"actor,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// WorkItemDetail is a work item with its timeline, runs and pending inbox items.
type WorkItemDetail struct {
	WorkItem
	Timeline []WorkItemEvent `json:"timeline"`
	Runs     []Run           `json:"runs"`
	Inbox    []InboxItem     `json:"inbox"`
}

// Principal is a user, a service account or a factory.
type Principal struct {
	Type string `json:"type"` // user, service_account or factory
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// InboxQuestion is one question of an inbox item.
type InboxQuestion struct {
	QuestionID        string   `json:"question_id"`
	Text              string   `json:"text"`
	Type              string   `json:"type"` // single_select, multi_select or text
	Options           []string `json:"options,omitempty"`
	RecommendedOption string   `json:"recommended_option,omitempty"`
}

// InboxAnswer is the answer to one question of an inbox item.
type InboxAnswer struct {
	QuestionID string   `json:"question_id"`
	Selected   []string `json:"selected,omitempty"`
	Text       string   `json:"text,omitempty"`
}

// InboxItem is a notice for people about one run.
type InboxItem struct {
	ID         string `json:"id"`
	FactoryID  string `json:"factory_id"`
	WorkItemID string `json:"work_item_id,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	// Kind is question, approval, handoff, review, failure, blocked,
	// answered or spend_limit.
	Kind      string          `json:"kind"`
	Title     string          `json:"title"`
	Body      string          `json:"body"`
	Options   []string        `json:"options,omitempty"`
	Questions []InboxQuestion `json:"questions,omitempty"`
	// Recipients are the users that the item is for. Empty means the
	// editors and admins of the factory.
	Recipients  []string   `json:"recipients"`
	IsRead      bool       `json:"is_read"`
	Resolved    bool       `json:"resolved"`
	Resolution  string     `json:"resolution,omitempty"`
	ResolvedBy  string     `json:"resolved_by,omitempty"`
	ResolvedVia string     `json:"resolved_via,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
}

// Webhook is a custom webhook of a factory.
type Webhook struct {
	ID               string    `json:"id"`
	FactoryID        string    `json:"factory_id"`
	Name             string    `json:"name"`
	AuthMode         string    `json:"auth_mode"`
	SignatureScheme  string    `json:"signature_scheme,omitempty"`
	SecretName       string    `json:"secret_name"`
	DeliveryIDHeader string    `json:"delivery_id_header,omitempty"`
	Enabled          bool      `json:"enabled"`
	IngressURL       string    `json:"ingress_url"`
	CreatedAt        time.Time `json:"created_at"`
}

// WebhookCreated is returned once at creation; Secret is not shown again.
type WebhookCreated struct {
	Webhook        Webhook `json:"webhook"`
	IngressURL     string  `json:"ingress_url"`
	Secret         string  `json:"secret,omitempty"`
	AwaitingSecret bool    `json:"awaiting_secret,omitempty"`
}

// Delivery is one webhook delivery.
type Delivery struct {
	ID         string            `json:"id"`
	WebhookID  string            `json:"webhook_id"`
	DeliveryID string            `json:"delivery_id,omitempty"`
	Status     string            `json:"status"`
	Label      string            `json:"label"`
	Reason     string            `json:"reason,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       json.RawMessage   `json:"body,omitempty"`
	Size       int               `json:"size"`
	RunIDs     []string          `json:"run_ids,omitempty"`
	ReceivedAt time.Time         `json:"received_at"`
}

// TestFilterResult is the outcome of testing a webhook trigger filter.
type TestFilterResult struct {
	WouldTrigger bool   `json:"would_trigger"`
	Result       string `json:"result"`
	WebhookCheck struct {
		Passed bool   `json:"passed"`
		Detail string `json:"detail"`
	} `json:"webhook_check"`
	PayloadCheck struct {
		Passed bool   `json:"passed"`
		Detail string `json:"detail"`
	} `json:"payload_check"`
	DeliveryID string `json:"delivery_id,omitempty"`
}

// Secret is secret metadata (values are write-only).
type Secret struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	FactoryID string    `json:"factory_id,omitempty"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Environment is a reusable workspace definition.
type Environment struct {
	ID            string            `json:"id"`
	TeamID        string            `json:"team_id"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Repos         []string          `json:"repos"`
	Image         string            `json:"image,omitempty"`
	SetupCommands []string          `json:"setup_commands,omitempty"`
	EnvVars       map[string]string `json:"env_vars,omitempty"`
}

// Machine is a compute spec declared by a factory definition.
type Machine struct {
	FactoryID     string   `json:"factory_id"`
	FactoryName   string   `json:"factory_name"`
	Name          string   `json:"name"`
	Description   string   `json:"description,omitempty"`
	OS            string   `json:"os"`
	Arch          string   `json:"arch"`
	Image         string   `json:"image,omitempty"`
	VCPUs         int      `json:"vcpus,omitempty"`
	MemoryGB      int      `json:"memoryGb,omitempty"`
	SetupCommands []string `json:"setupCommands,omitempty"`
}

// Worker is a registered execution host.
type Worker struct {
	ID            string            `json:"id"`
	TeamID        string            `json:"team_id"`
	Name          string            `json:"name"`
	Backend       string            `json:"backend"`
	Labels        map[string]string `json:"labels,omitempty"`
	Capacity      int               `json:"capacity"`
	Active        int               `json:"active"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
	Version       string            `json:"version,omitempty"`
	Status        string            `json:"status"`
}

// Integration is a team-level connection to an external system.
type Integration struct {
	ID        string            `json:"id"`
	TeamID    string            `json:"team_id"`
	Provider  string            `json:"provider"`
	Name      string            `json:"name"`
	Config    map[string]string `json:"config"`
	SecretRef string            `json:"secret_ref,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
}

// APIKey is API key metadata.
type APIKey struct {
	ID         string     `json:"id"`
	TeamID     string     `json:"team_id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// APIKeyCreated carries a new key's secret, shown once.
type APIKeyCreated struct {
	APIKey APIKey `json:"api_key"`
	Key    string `json:"key"`
}

// User is a team member.
type User struct {
	ID        string    `json:"id"`
	TeamID    string    `json:"team_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
}

// Team is a tenant.
type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Me describes the authenticated caller.
type Me struct {
	User   User   `json:"user"`
	Team   Team   `json:"team"`
	Role   string `json:"role"`
	APIKey APIKey `json:"api_key"`
}

// Scorer is an LLM judge configuration.
type Scorer struct {
	ID              string            `json:"id"`
	FactoryID       string            `json:"factory_id"`
	Slug            string            `json:"slug"`
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Agents          []json.RawMessage `json:"agents"`
	Labels          []json.RawMessage `json:"labels"`
	PassingScore    float64           `json:"passing_score"`
	SamplingRate    float64           `json:"sampling_rate"`
	Model           string            `json:"model"`
	SelfImprovement bool              `json:"self_improvement"`
}

// Score is one scorer verdict.
type Score struct {
	ID               string    `json:"id"`
	ScorerID         string    `json:"scorer_id"`
	RunID            string    `json:"run_id"`
	AgentName        string    `json:"agent_name"`
	Label            string    `json:"label"`
	Score            float64   `json:"score"`
	Passed           bool      `json:"passed"`
	Rationale        string    `json:"rationale"`
	CreatedAt        time.Time `json:"created_at"`
	ImprovementRunID string    `json:"improvement_run_id,omitempty"`
}

// BenchmarkSuite is a benchmark declared in a factory definition.
type BenchmarkSuite struct {
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	Description    string            `json:"description,omitempty"`
	Agent          string            `json:"agent"`
	Configurations []json.RawMessage `json:"configurations,omitempty"`
	Tasks          []json.RawMessage `json:"tasks,omitempty"`
}

// BenchmarkRun is one launch of a benchmark suite.
type BenchmarkRun struct {
	ID         string          `json:"id"`
	FactoryID  string          `json:"factory_id"`
	Suite      string          `json:"suite"`
	State      string          `json:"state"`
	Results    json.RawMessage `json:"results,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// Metrics wraps a factory's dashboard metrics.
type Metrics struct {
	FactoryID string          `json:"factory_id"`
	Since     time.Time       `json:"since"`
	Metrics   json.RawMessage `json:"metrics"`
}

// SendMessageParams is the body of RunsService.SendMessage.
type SendMessageParams struct {
	Message string
	// Attachments are attachment IDs from UploadAttachment.
	Attachments []string
}

// SendMessageResult is the answer to a message sent to a run.
type SendMessageResult struct {
	RunID     string `json:"run_id"`
	MessageID string `json:"message_id"`
	Outcome   string `json:"outcome"`
}

// Attachment is an uploaded file that a message can reference.
type Attachment struct {
	ID          string    `json:"id"`
	TeamID      string    `json:"team_id,omitempty"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Bytes       int64     `json:"bytes"`
	CreatedBy   string    `json:"created_by,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// TimelineEvent is one event of the timeline of a run.
type TimelineEvent struct {
	EventID string          `json:"event_id"`
	RunID   string          `json:"run_id"`
	Session int             `json:"session"`
	Type    string          `json:"type"`
	At      time.Time       `json:"at"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Diagnostic is one diagnostic of a run.
type Diagnostic struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	At      time.Time       `json:"at"`
	Session int             `json:"session"`
}

// RunSession is one session of a run.
type RunSession struct {
	Session       int        `json:"session"`
	Number        int        `json:"number"`
	State         string     `json:"state"`
	AssignedAt    time.Time  `json:"assigned_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	ReadyAt       *time.Time `json:"ready_at,omitempty"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	EndReason     string     `json:"end_reason,omitempty"`
	FailureCode   string     `json:"failure_code,omitempty"`
	SandboxReason string     `json:"sandbox_reason,omitempty"`
	Usage         Usage      `json:"usage"`
	Snapshot      struct {
		Workspace    bool `json:"workspace"`
		HarnessState bool `json:"harness_state"`
	} `json:"snapshot"`
}

// RunInput is one message in the queue of a run.
type RunInput struct {
	MessageID   string          `json:"message_id"`
	Seq         int64           `json:"seq"`
	Kind        string          `json:"kind"`
	Sender      json.RawMessage `json:"sender,omitempty"`
	State       string          `json:"state"`
	Content     string          `json:"content"`
	Data        json.RawMessage `json:"data,omitempty"`
	Attachments []string        `json:"attachments,omitempty"`
	Untrusted   bool            `json:"untrusted,omitempty"`
	DedupeKey   string          `json:"dedupe_key,omitempty"`
	Session     int             `json:"session,omitempty"`
	TurnID      string          `json:"turn_id,omitempty"`
	Accepted    bool            `json:"accepted"`
	CreatedAt   time.Time       `json:"created_at"`
}

// Conversation is the normalized conversation of a run. The blocks of a
// message stay raw JSON, because their shape depends on the block type.
type Conversation struct {
	ConversationID string             `json:"conversation_id"`
	Steps          []ConversationStep `json:"steps"`
}

// ConversationStep is one step of a conversation.
type ConversationStep struct {
	StepID   string                `json:"step_id"`
	TurnID   string                `json:"turn_id,omitempty"`
	Session  int                   `json:"session,omitempty"`
	Messages []ConversationMessage `json:"messages"`
}

// ConversationMessage is one message of a conversation step.
type ConversationMessage struct {
	MessageID string          `json:"message_id,omitempty"`
	Role      string          `json:"role"`
	Kind      string          `json:"kind,omitempty"`
	Sender    json.RawMessage `json:"sender,omitempty"`
	Blocks    json.RawMessage `json:"blocks"`
}
