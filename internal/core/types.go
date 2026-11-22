// Package core holds the shared domain types and interfaces of Open Factory.
// It is the contract between packages; keep it dependency-free (stdlib only).
package core

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// ---------- Identity ----------

type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID     string `json:"id"`
	TeamID string `json:"team_id"`
	Email  string `json:"email"`
	Name   string `json:"name"`
	Role   Role   `json:"role"`
	// FactoryRoles holds role overrides for single factories, keyed by
	// factory ID. For a request about a factory, the override replaces the
	// team-wide Role.
	FactoryRoles map[string]Role `json:"factory_roles,omitempty"`
	// Kind is UserKindPerson (empty) or UserKindServiceAccount. A
	// service account has an ID that starts with "sa_", no email and no
	// sign-in. Only its API keys (prefix "ofa_") authenticate it.
	Kind      string    `json:"kind,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Role string

const (
	RoleAdmin  Role = "admin"
	RoleEditor Role = "editor"
	// RoleUser is the lowest role: it can view everything and run work
	// (dispatch runs, send messages, resolve inbox items).
	RoleUser Role = "user"
)

// NormalizeRole trims and lowercases the name of a role.
func NormalizeRole(r Role) Role {
	return Role(strings.ToLower(strings.TrimSpace(string(r))))
}

// RoleRank orders the built-in roles: user 1, editor 2, admin 3, and 0 for
// unknown names.
func RoleRank(r Role) int {
	switch NormalizeRole(r) {
	case RoleAdmin:
		return 3
	case RoleEditor:
		return 2
	case RoleUser:
		return 1
	}
	return 0
}

// RoleFor returns the role of the user for one factory: the override for
// that factory if one exists, else the team-wide role. An empty factoryID
// returns the team-wide role. The result is normalized.
func (u *User) RoleFor(factoryID string) Role {
	if u == nil {
		return ""
	}
	if factoryID != "" {
		if r, ok := u.FactoryRoles[factoryID]; ok && RoleRank(r) > 0 {
			return NormalizeRole(r)
		}
	}
	return NormalizeRole(u.Role)
}

// ExternalIdentity links a user to an account on an integration (GitHub login, Slack user id...).
type ExternalIdentity struct {
	UserID     string `json:"user_id"`
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Handle     string `json:"handle"`
}

type APIKey struct {
	ID         string     `json:"id"`
	TeamID     string     `json:"team_id"`
	UserID     string     `json:"user_id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	HashedKey  string     `json:"-"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// ---------- Factory ----------

type DefinitionSource string

const (
	DefinitionManaged DefinitionSource = "managed"
	DefinitionGit     DefinitionSource = "git"
)

type Factory struct {
	ID          string           `json:"id"`
	TeamID      string           `json:"team_id"`
	Name        string           `json:"name"`
	Alias       string           `json:"alias"`
	Description string           `json:"description,omitempty"`
	Source      DefinitionSource `json:"source"`
	// Git-backed definitions
	GitRepo         string    `json:"git_repo,omitempty"`          // e.g. github.com/acme/factory-def
	GitBranch       string    `json:"git_branch,omitempty"`        // production branch, default main
	GitPath         string    `json:"git_path,omitempty"`          // subdir holding factory.yaml
	ActiveVersionID string    `json:"active_version_id,omitempty"` // last valid applied definition
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// DefinitionVersion is one immutable snapshot of a definition tree.
type DefinitionVersion struct {
	ID        string            `json:"id"`
	FactoryID string            `json:"factory_id"`
	Files     map[string]string `json:"files"` // path -> content
	CommitSHA string            `json:"commit_sha,omitempty"`
	Author    string            `json:"author,omitempty"`
	Message   string            `json:"message,omitempty"`
	Valid     bool              `json:"valid"`
	Errors    []ValidationError `json:"errors,omitempty"`
	AppliedAt *time.Time        `json:"applied_at,omitempty"`
	CreatedAt time.Time         `json:"created_at"`
	// State is set for a proposed version (DefinitionProposed, then
	// DefinitionApproved or DefinitionRejected). Empty for other versions:
	// see EffectiveState.
	State string `json:"state,omitempty"`
	// BaseVersionID is the active version when the proposal was made.
	BaseVersionID string `json:"base_version_id,omitempty"`
	// AuthorRunID is the run that proposed the version.
	AuthorRunID string `json:"author_run_id,omitempty"`
	// InboxItemID is the review item of the proposal.
	InboxItemID string `json:"inbox_item_id,omitempty"`
	// AppliedVersionID is the version that an approval applied.
	AppliedVersionID string     `json:"applied_version_id,omitempty"`
	ReviewedBy       string     `json:"reviewed_by,omitempty"`
	ReviewComment    string     `json:"review_comment,omitempty"`
	ReviewedAt       *time.Time `json:"reviewed_at,omitempty"`
}

type ValidationError struct {
	File    string `json:"file"`
	Line    int    `json:"line,omitempty"`
	Field   string `json:"field,omitempty"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string { return e.File + ": " + e.Message }

// ---------- Agents (resolved/effective configuration) ----------

type AgentType string

const (
	AgentForeman   AgentType = "FOREMAN"
	AgentTriage    AgentType = "TRIAGE"
	AgentSpec      AgentType = "SPEC"
	AgentImplement AgentType = "IMPLEMENT"
	AgentReview    AgentType = "REVIEW"
	AgentVerify    AgentType = "VERIFY"
	AgentCustom    AgentType = "CUSTOM"
)

// BuiltinMCPPrefix is the managedId namespace of the MCP servers that the
// control plane serves itself (integration tools such as builtin/linear).
const BuiltinMCPPrefix = "builtin/"

// BuiltinMCPProviders are the integrations with a built-in MCP server.
var BuiltinMCPProviders = []string{"jira", "linear", "slack", "teams"}

// BuiltinIntegrationProvider returns the provider name of the integration
// record that a built-in server uses ("teams" -> "microsoft_teams"). The
// other servers use their own name.
func BuiltinIntegrationProvider(provider string) string {
	if provider == "teams" {
		return "microsoft_teams"
	}
	return provider
}

// SameIntegrationProvider reports whether two provider names name the same
// integration. It ignores case and the difference between "-" and "_", and it
// treats "teams" as "microsoft_teams".
func SameIntegrationProvider(a, b string) bool {
	norm := func(v string) string {
		v = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(v), "-", "_"))
		if v == "teams" {
			return "microsoft_teams"
		}
		return v
	}
	return norm(a) == norm(b)
}

// BuiltinMCPProvider returns the provider of a built-in managedId
// ("builtin/linear" -> "linear", true). ok is false for other ids, and for
// builtin ids naming an unknown provider (known is then false too).
func BuiltinMCPProvider(managedID string) (provider string, ok bool) {
	p, found := strings.CutPrefix(managedID, BuiltinMCPPrefix)
	if !found {
		return "", false
	}
	for _, k := range BuiltinMCPProviders {
		if k == p {
			return p, true
		}
	}
	return p, false
}

// RegisteredMCPServer is an entry of a team's MCP server registry. Agents
// reference it with mcpServers.<name>.managedId set to its Name (or ID).
// Exactly one of URL and Command is set. Env and Headers values may hold
// {{SECRET}} placeholders; literal credentials are rejected by the API.
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
	// Connection is how a run reaches a url server: MCPConnectionProxy (the
	// default) routes it through the control plane (/mcp/proxy/{run}/{id}),
	// so headers and OAuth tokens are added there and never enter the
	// sandbox. MCPConnectionDirect sends the expanded headers to the
	// sandbox, for a server that only the sandbox can reach. Empty for
	// command servers. Use Proxied to read it.
	Connection string          `json:"connection,omitempty"`
	OAuth      *MCPOAuthConfig `json:"oauth,omitempty"`
	CreatedBy  string          `json:"created_by,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// Values of RegisteredMCPServer.Connection.
const (
	MCPConnectionProxy  = "proxy"
	MCPConnectionDirect = "direct"
)

// Proxied reports whether runs reach the server through the control-plane
// proxy.
func (m *RegisteredMCPServer) Proxied() bool {
	return m.URL != "" && m.Connection == MCPConnectionProxy
}

// MCP OAuth connection states.
const (
	MCPOAuthNotConnected = "not_connected"
	MCPOAuthConnected    = "connected"
	MCPOAuthError        = "error"
)

// MCPOAuthConfig configures an OAuth-protected registered server. Endpoints
// and the client are discovered (protected resource metadata, authorization
// server metadata, dynamic client registration) unless set here.
type MCPOAuthConfig struct {
	Scopes                []string `json:"scopes,omitempty"`
	ClientID              string   `json:"client_id,omitempty"`
	ClientSecretName      string   `json:"client_secret_name,omitempty"` // team secret holding the client secret
	AuthorizationEndpoint string   `json:"authorization_endpoint,omitempty"`
	TokenEndpoint         string   `json:"token_endpoint,omitempty"`

	// Connection state, maintained by the server.
	Status      string     `json:"status"`
	ConnectedBy string     `json:"connected_by,omitempty"`
	ConnectedAt *time.Time `json:"connected_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type NetworkPolicy struct {
	Extends      []string `json:"extends,omitempty"`
	AllowedHosts []string `json:"allowedHosts,omitempty"`
	DeniedHosts  []string `json:"deniedHosts,omitempty"`
}

type MachineSpec struct {
	Name                         string   `json:"name"`
	Description                  string   `json:"description,omitempty"`
	SetupCommands                []string `json:"setupCommands,omitempty"`
	VCPUs                        int      `json:"vcpus,omitempty"`
	MemoryGB                     int      `json:"memoryGb,omitempty"`
	OS                           string   `json:"os"`   // linux|macos|windows
	Arch                         string   `json:"arch"` // x86_64|aarch64
	Image                        string   `json:"image,omitempty"`
	MacVersion                   string   `json:"macVersion,omitempty"`
	RegistryCredentialSecretName string   `json:"registryCredentialSecretName,omitempty"`
	// User is platform.linux.user: a numeric user ID, or "image" to keep the
	// user of the image. Empty means the backend default (1000).
	User                           string `json:"user,omitempty"`
	FailureSessionRetentionMinutes int    `json:"failureSessionRetentionMinutes,omitempty"`
}

// AgentConfig is the fully resolved configuration of one agent (defaults + overrides applied).
type AgentConfig struct {
	ID                 string                 `json:"id"` // stable agent identity UID
	FactoryID          string                 `json:"factory_id"`
	Name               string                 `json:"name"`
	Description        string                 `json:"description"`
	Type               AgentType              `json:"type"`
	Instructions       string                 `json:"instructions"`
	Harness            HarnessConfig          `json:"harness"`
	Machine            *MachineSpec           `json:"machine,omitempty"`
	EnvironmentID      string                 `json:"environment_id,omitempty"`
	WorkerHost         string                 `json:"worker_host,omitempty"`
	Secrets            []string               `json:"secrets,omitempty"`
	MCPServers         map[string]MCPServer   `json:"mcp_servers,omitempty"`
	NetworkPolicy      EffectiveNetworkPolicy `json:"network_policy"`
	SpawnableBy        []string               `json:"spawnable_by"` // nil => foreman only
	IdleTimeoutMinutes int                    `json:"idle_timeout_minutes,omitempty"`
	// IdleTimeoutSet is true when the definition or an override set
	// IdleTimeoutMinutes. Then 0 means "end the session after the turn".
	IdleTimeoutSet     bool    `json:"idle_timeout_set,omitempty"`
	CredentialStrategy string  `json:"credential_strategy,omitempty"` // EXECUTOR|CREATOR
	Skills             []Skill `json:"skills,omitempty"`
	// Snapshot is the snapshot setting of the agent. Nil and true store both
	// snapshot parts. False stores only the harness_state part.
	Snapshot *bool `json:"snapshot,omitempty"`
}

// ---------- Automations ----------

type Trigger struct {
	Provider string          `json:"provider"` // github, gitlab, azure_devops, slack, microsoft_teams, linear, jira, webhook, schedule
	Event    string          `json:"event"`
	Filter   json.RawMessage `json:"filter,omitempty"`
	Schedule *Schedule       `json:"schedule,omitempty"`
	// Concurrency is for schedule triggers only: ScheduleConcurrencyAllow
	// (the default, also when empty) starts a run for every slot;
	// ScheduleConcurrencySkip drops a slot while a run of the same
	// automation is not finished.
	Concurrency string `json:"concurrency,omitempty"`
}

// Values of Trigger.Concurrency.
const (
	ScheduleConcurrencyAllow = "allow"
	ScheduleConcurrencySkip  = "skip"
)

type Schedule struct {
	Name string `json:"name,omitempty"`
	Cron string `json:"cron"`
}

type Automation struct {
	ID          string    `json:"id"`
	FactoryID   string    `json:"factory_id"`
	Name        string    `json:"name"`
	DisplayName string    `json:"display_name,omitempty"`
	Enabled     bool      `json:"enabled"`
	Agent       string    `json:"agent"` // agent name; default foreman
	Prompt      string    `json:"prompt"`
	Triggers    []Trigger `json:"triggers"`
	// RunAs is the principal of the runs that the automation starts:
	// RunAsFactory (the default, empty), "user:<email>" or
	// "serviceAccount:<name>".
	RunAs string `json:"run_as,omitempty"`
	// Overrides applied on top of the target agent's AgentConfig (nil = inherit)
	Overrides *AgentOverrides `json:"overrides,omitempty"`
}

// AgentOverrides carries sparse execution overrides (automation level or run_agents level).
type AgentOverrides struct {
	Harness       *HarnessConfig `json:"harness,omitempty"`
	Model         string         `json:"model,omitempty"`
	Machine       string         `json:"machine,omitempty"`
	EnvironmentID *string        `json:"environment_id,omitempty"`
	// Secrets and MCPServers replace the agent's lists when non-nil (an empty,
	// non-nil value clears them); factory-wide entries always still apply.
	// They are not omitempty so that an explicit empty list survives storage.
	Secrets            []string             `json:"secrets"`
	MCPServers         map[string]MCPServer `json:"mcp_servers"`
	WorkerHost         *string              `json:"worker_host,omitempty"`
	IdleTimeoutMinutes *int                 `json:"idle_timeout_minutes,omitempty"`
}

// ---------- Events (normalized from every source) ----------

// Event is a normalized inbound event from any work source.
type Event struct {
	ID         string          `json:"id"`
	TeamID     string          `json:"team_id"`
	FactoryID  string          `json:"factory_id,omitempty"` // set when the source is factory-scoped (webhook, API, mention of alias)
	Provider   string          `json:"provider"`             // see Trigger.Provider
	Type       string          `json:"type"`                 // provider event string, e.g. issue_labeled, pull_request_opened, app_mention, received, cron_fired
	DeliveryID string          `json:"delivery_id,omitempty"`
	ActionKey  string          `json:"action_key,omitempty"` // same for all events of one provider action; derived from DeliveryID when empty
	Actor      Actor           `json:"actor"`
	Source     SourceRef       `json:"source"`
	Attributes map[string]any  `json:"attributes"` // filterable attributes (repo, labels, branch, channel, keywords...)
	Payload    json.RawMessage `json:"payload,omitempty"`
	Text       string          `json:"text,omitempty"` // human text (message body, issue body, comment)
	ReceivedAt time.Time       `json:"received_at"`
}

type Actor struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	Handle     string `json:"handle"`
	IsBot      bool   `json:"is_bot"`
	UserID     string `json:"user_id,omitempty"` // linked Open Factory user, if any
	// IsSelf is true for the product's own bot or app. The engine drops its actions.
	IsSelf bool `json:"is_self,omitempty"`
	// Trust is set by the engine: member, external or platform.
	Trust string `json:"trust,omitempty"`
}

// Sender trust values (Actor.Trust).
const (
	TrustMember   = "member"
	TrustExternal = "external"
	TrustPlatform = "platform"
)

// SourceRef identifies where a work item came from, so results return there.
type SourceRef struct {
	Provider   string `json:"provider"` // github, slack, linear, api, mcp, webhook, schedule...
	Kind       string `json:"kind"`     // issue, pull_request, thread, work_item, delivery, task
	URL        string `json:"url,omitempty"`
	Repo       string `json:"repo,omitempty"`    // owner/name
	Number     int    `json:"number,omitempty"`  // issue / PR number
	Channel    string `json:"channel,omitempty"` // chat channel / conversation id
	ThreadID   string `json:"thread_id,omitempty"`
	ExternalID string `json:"external_id,omitempty"` // tracker key, e.g. ENG-123
	Title      string `json:"title,omitempty"`
	// ThreadRef names a sub-thread of the source, for example the ID of the
	// first comment of a review thread. A reply goes into that thread. It is
	// not part of Key: the events of a thread route to the work item of the
	// source.
	ThreadRef string `json:"thread_ref,omitempty"`
	// Workspace is the workspace, team or site of a chat or tracker source:
	// the value of the selection key of its integration record (Slack
	// team_id, Teams team_id, Linear organization_id, Jira site). A reply
	// uses it to select the record. It is not part of
	// Key.
	Workspace string `json:"workspace,omitempty"`
}

// Key returns a stable identity used to dedupe/attach events to an existing work item.
func (s SourceRef) Key() string {
	switch {
	case s.Repo != "" && s.Number != 0:
		return s.Provider + ":" + s.Repo + "#" + itoa(s.Number)
	case s.ThreadID != "":
		return s.Provider + ":" + s.Channel + ":" + s.ThreadID
	case s.ExternalID != "":
		return s.Provider + ":" + s.ExternalID
	default:
		return ""
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// ---------- Work items ----------

type Stage string

const (
	StageIntake           Stage = "intake"
	StageTriage           Stage = "triage"
	StagePlanning         Stage = "planning"
	StageAwaitingApproval Stage = "awaiting_approval"
	StageBuilding         Stage = "building"
	StageReviewing        Stage = "reviewing"
	StageHandoff          Stage = "handoff"
	StageComplete         Stage = "complete"
	StageCancelled        Stage = "cancelled"
)

func (s Stage) Terminal() bool { return s == StageComplete || s == StageCancelled }

type WorkItem struct {
	ID           string           `json:"id"`
	FactoryID    string           `json:"factory_id"`
	Title        string           `json:"title"`
	Stage        Stage            `json:"stage"`
	Source       SourceRef        `json:"source"`
	SourceKey    string           `json:"source_key,omitempty"`
	AutomationID string           `json:"automation_id,omitempty"`
	CreatorID    string           `json:"creator_id,omitempty"`
	RootRunID    string           `json:"root_run_id,omitempty"` // foreman run
	PullRequests []PullRequestRef `json:"pull_requests,omitempty"`
	// References are the external objects that route events to the work
	// item: the source, tracker issues, pull requests and chat threads.
	// See WorkItem.Refs.
	References  []WorkItemRef     `json:"references,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	LocalOwner  string            `json:"local_owner,omitempty"` // set when taken over locally via Factory MCP
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	CompletedAt *time.Time        `json:"completed_at,omitempty"`
	// Version is the version of the stored row. UpdateWorkItem checks it and
	// adds 1.
	Version int64 `json:"version"`
}

type PullRequestRef struct {
	Forge  string `json:"forge"`
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	State  string `json:"state"` // open, draft, merged, closed
}

// WorkItemEvent is the timeline of a work item (stage changes, messages, decisions).
type WorkItemEvent struct {
	ID         string          `json:"id"`
	WorkItemID string          `json:"work_item_id"`
	Kind       string          `json:"kind"` // stage_changed, message_in, message_out, run_started, run_finished, approval_requested, approval_decided, question, answer, pr_linked, note
	RunID      string          `json:"run_id,omitempty"`
	Actor      string          `json:"actor,omitempty"`
	Data       json.RawMessage `json:"data,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// ---------- Runs ----------

type RunState string

const (
	RunQueued    RunState = "queued"
	RunStarting  RunState = "starting"
	RunRunning   RunState = "running"
	RunIdle      RunState = "idle" // finished a turn; session kept for follow-ups until idle timeout
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCancelled RunState = "cancelled"

	// States of the run model. The run state machine
	// (internal/runstate) sets them. Nothing sets RunStarting and RunIdle any
	// more; they stay only so that older stored runs still read.
	RunPending RunState = "pending" // a backend prepares the sandbox
	RunClaimed RunState = "claimed" // the runner does its setup
	RunBlocked RunState = "blocked" // the last turn ended with an open request
	RunError   RunState = "error"   // the run ended because of the platform
)

// Terminal tells that the state is a finished state. A finished run can
// continue with a new message.
func (s RunState) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCancelled || s == RunError
}

// WaitsForInput tells that the run finished a turn and waits for a message,
// but is not finished: blocked (idle is the old name that earlier releases stored).
func (s RunState) WaitsForInput() bool {
	return s == RunIdle || s == RunBlocked
}

type Run struct {
	ID string `json:"id"`
	// Version is the version of the stored row. UpdateRun checks it and adds 1.
	Version     int64             `json:"version"`
	TeamID      string            `json:"team_id"`
	FactoryID   string            `json:"factory_id"` // each run belongs to a factory
	WorkItemID  string            `json:"work_item_id,omitempty"`
	ParentRunID string            `json:"parent_run_id,omitempty"`
	AgentID     string            `json:"agent_id,omitempty"`
	AgentName   string            `json:"agent_name,omitempty"`
	Name        string            `json:"name,omitempty"` // label (config name / child name)
	State       RunState          `json:"state"`
	Prompt      string            `json:"prompt"`
	Config      AgentConfig       `json:"config"` // snapshot of effective config at launch
	Metadata    map[string]string `json:"metadata,omitempty"`
	SessionID   string            `json:"session_id,omitempty"` // harness session id for resume
	ConsumedSeq int64             `json:"consumed_seq"`         // last user RunMessage.Seq already fed to the harness
	SandboxID   string            `json:"sandbox_id,omitempty"`
	WorkerID    string            `json:"worker_id,omitempty"`
	Result      string            `json:"result,omitempty"` // final message / summary
	Error       string            `json:"error,omitempty"`
	// FailureCode is set when State is failed or error.
	FailureCode FailureCode `json:"failure_code,omitempty"`
	// BlockedReason is set when State is blocked: "human_request" or "harness_needs_input".
	BlockedReason string `json:"blocked_reason,omitempty"`
	// QueuedReason is set when State is queued and the run waits for a
	// reason other than capacity: QueuedSpendLimit.
	QueuedReason string `json:"queued_reason,omitempty"`
	// Turns is the number of turns that ended, on the runner path. Together
	// with ConsumedSeq it identifies one turn end.
	Turns int64 `json:"turns,omitempty"`
	// CancelledBy is the actor of the cancel request, when State is cancelled.
	CancelledBy string `json:"cancelled_by,omitempty"`
	Usage       Usage  `json:"usage"`
	TriggeredBy string `json:"triggered_by,omitempty"` // automation:<id>, schedule:<id>, webhook:<id>, api, mcp, run:<id>, benchmark:<id>, scorer:<id>
	// Source and SourceRef are the stored source of the run. The
	// engine sets them when it creates the run. A run from
	// before these fields has them empty: use SourceAndRef, which falls back
	// to TriggeredBy.
	Source    RunSource `json:"source,omitempty"`
	SourceRef string    `json:"source_ref,omitempty"`
	// Creator is who caused the run. It is nil for a schedule, a webhook, a
	// scorer or a benchmark. A child run has the
	// creator of its parent.
	Creator *Principal `json:"creator,omitempty"`
	// RunAs is who the run acts as. It is set on each run that the engine
	// creates. It is nil only for runs from before this field.
	RunAs      *Principal `json:"run_as,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// QueuedAt is the time of the last move to queued on the runner path. Nil
	// means CreatedAt. The queue timeout counts from it.
	QueuedAt *time.Time `json:"queued_at,omitempty"`
}

// RunMessage is one entry in a run's transcript/conversation.
type RunMessage struct {
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	Seq       int64           `json:"seq"`
	Role      string          `json:"role"` // user, assistant, tool_call, tool_result, system, log
	Content   string          `json:"content"`
	Data      json.RawMessage `json:"data,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// ---------- Inbox ----------

// QueuedSpendLimit is the queued reason of a run that waits because the
// spend of its factory or team is at the limit.
const QueuedSpendLimit = "spend_limit"

type InboxKind string

const (
	InboxApproval InboxKind = "approval"
	InboxQuestion InboxKind = "question"
	InboxHandoff  InboxKind = "handoff"
	InboxFailure  InboxKind = "failure"
	InboxReview   InboxKind = "review" // self-improvement / definition PR to review
	// InboxSpendLimit tells an admin that 80 percent of a spend limit is used.
	InboxSpendLimit InboxKind = "spend_limit"
	// InboxBlocked is for a run in blocked with the reason harness_needs_input.
	InboxBlocked InboxKind = "blocked"
	// InboxAnswered is information only. It needs no answer.
	InboxAnswered InboxKind = "answered"
)

// Question types of an inbox question.
const (
	QuestionSingleSelect = "single_select"
	QuestionMultiSelect  = "multi_select"
	QuestionText         = "text"
)

// Question is one question of an inbox item.
type Question struct {
	ID                string   `json:"question_id"`
	Text              string   `json:"text"`
	Type              string   `json:"type"` // single_select, multi_select or text
	Options           []string `json:"options,omitempty"`
	RecommendedOption string   `json:"recommended_option,omitempty"`
}

type InboxItem struct {
	ID         string    `json:"id"`
	FactoryID  string    `json:"factory_id"`
	WorkItemID string    `json:"work_item_id,omitempty"`
	RunID      string    `json:"run_id,omitempty"`
	Kind       InboxKind `json:"kind"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Options    []string  `json:"options,omitempty"`
	// Questions are the questions of a question item, each with an ID, a
	// type and a recommended option.
	Questions []Question `json:"questions,omitempty"`
	// AssigneeID is the old single recipient. Recipients replaces it, and
	// RecipientIDs reads both.
	AssigneeID string `json:"assignee_id,omitempty"`
	// Recipients are the users that the item is for. The default is the
	// creator of the work item. An item without recipients is for the
	// editors and admins of the factory.
	Recipients []string `json:"recipients,omitempty"`
	// ReadBy holds the users that marked the item as read.
	ReadBy []string `json:"read_by,omitempty"`
	// AnswerMessageID is the run message that carried the resolution to the
	// run. An unresolve checks that the run did not consume it.
	AnswerMessageID string `json:"answer_message_id,omitempty"`
	Resolved        bool   `json:"resolved"`
	Resolution      string `json:"resolution,omitempty"`
	ResolvedBy      string `json:"resolved_by,omitempty"`
	// ResolvedVia is the tool the item was resolved from: one of the
	// InboxVia* values. Empty for items the engine closed itself.
	ResolvedVia string     `json:"resolved_via,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	// Version is the version of the stored row. UpdateInboxItem checks it and
	// adds 1.
	Version int64 `json:"version"`
}

// RecipientIDs returns the recipients of the item: Recipients, or the old
// AssigneeID when Recipients is empty.
func (i *InboxItem) RecipientIDs() []string {
	if len(i.Recipients) > 0 {
		return i.Recipients
	}
	if i.AssigneeID != "" {
		return []string{i.AssigneeID}
	}
	return nil
}

// IsReadBy tells that the user marked the item as read.
func (i *InboxItem) IsReadBy(userID string) bool {
	for _, u := range i.ReadBy {
		if u == userID {
			return true
		}
	}
	return false
}

// ---------- Secrets ----------

const (
	SecretGeneric        SecretType = "generic"
	SecretBedrockKey     SecretType = "anthropic_bedrock_api_key"    //nolint:gosec // G101: a secret type name, not a credential
	SecretBedrockAccess  SecretType = "anthropic_bedrock_access_key" //nolint:gosec // G101: a secret type name, not a credential
	SecretDockerRegistry SecretType = "docker_registry"
	SecretAWSECR         SecretType = "aws_ecr_credential" //nolint:gosec // G101: a secret type name, not a credential
)

type Secret struct {
	ID        string     `json:"id"`
	TeamID    string     `json:"team_id"`
	FactoryID string     `json:"factory_id,omitempty"` // empty = team scope
	Name      string     `json:"name"`
	Type      SecretType `json:"type"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// ---------- Webhooks ----------

type Webhook struct {
	ID               string    `json:"id"`
	FactoryID        string    `json:"factory_id"`
	Name             string    `json:"name"`
	AuthMode         string    `json:"auth_mode"` // token | url_token | signature
	SignatureScheme  string    `json:"signature_scheme,omitempty"`
	SecretName       string    `json:"secret_name"`
	DeliveryIDHeader string    `json:"delivery_id_header,omitempty"`
	Enabled          bool      `json:"enabled"`
	CreatedAt        time.Time `json:"created_at"`
}

type WebhookDelivery struct {
	ID         string            `json:"id"`
	WebhookID  string            `json:"webhook_id"`
	DeliveryID string            `json:"delivery_id,omitempty"`
	Status     string            `json:"status"` // accepted, rejected, deduplicated
	Reason     string            `json:"reason,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"` // credential headers stripped
	Body       json.RawMessage   `json:"body,omitempty"`
	RunIDs     []string          `json:"run_ids,omitempty"`
	ReceivedAt time.Time         `json:"received_at"`
}

// WebhookDeliveryDeduplicated is the status of a delivery log entry for a
// repeated delivery. Such entries share the delivery ID of the accepted
// entry; only one accepted entry exists per webhook and delivery ID.
const WebhookDeliveryDeduplicated = "deduplicated"

// ---------- Integrations ----------

type Integration struct {
	ID        string            `json:"id"`
	TeamID    string            `json:"team_id"`
	Provider  string            `json:"provider"` // github, gitlab, azure_devops, slack, microsoft_teams, linear, jira
	Name      string            `json:"name"`
	Config    map[string]string `json:"config"`               // non-secret config (installation id, workspace id, base url...)
	SecretRef string            `json:"secret_ref,omitempty"` // name of secret holding token
	// FactoryID binds the record to one factory of the team. Empty means
	// the team record. A record of a factory wins over the team record.
	FactoryID string `json:"factory_id,omitempty"`
	// Status is IntegrationActive (also when empty), IntegrationDisabled
	// or IntegrationAuthFailed.
	Status string `json:"status,omitempty"`
	// StatusReason tells why the status changed, for example "app removed".
	StatusReason    string     `json:"status_reason,omitempty"`
	StatusUpdatedAt *time.Time `json:"status_updated_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// Integration statuses.
const (
	IntegrationActive     = "active"
	IntegrationDisabled   = "disabled"
	IntegrationAuthFailed = "auth_failed"
)

// EffectiveStatus is the status of the record; empty means active.
func (i *Integration) EffectiveStatus() string {
	if i.Status == "" {
		return IntegrationActive
	}
	return i.Status
}

// Active reports whether the record can be used.
func (i *Integration) Active() bool { return i.EffectiveStatus() == IntegrationActive }

// ---------- Scoring / benchmarks ----------

type ScorerLabel struct {
	Value       string  `json:"value"`
	Description string  `json:"description,omitempty"`
	Score       float64 `json:"score"`
}

type Scorer struct {
	ID              string        `json:"id"`
	FactoryID       string        `json:"factory_id"`
	Slug            string        `json:"slug"`
	Name            string        `json:"name"`
	Description     string        `json:"description,omitempty"`
	Agents          []ScorerAgent `json:"agents"`
	Labels          []ScorerLabel `json:"labels"`
	PassingScore    float64       `json:"passing_score"`
	SamplingRate    float64       `json:"sampling_rate"`
	Model           string        `json:"model"`
	Rubric          string        `json:"rubric"`
	SelfImprovement bool          `json:"self_improvement"`
	// Execution settings that override the definition's scorerDefaults for this scorer.
	Machine    string               `json:"machine,omitempty"`
	Secrets    []string             `json:"secrets,omitempty"`
	MCPServers map[string]MCPServer `json:"mcp_servers,omitempty"`
	// Harness is the effective harness of the judge run: the harness
	// of the scorer, else of scorerDefaults, else of agentDefaults, with
	// Model applied. Nil for scorers compiled before the field existed.
	Harness *HarnessConfig `json:"harness,omitempty"`
}

type ScorerAgent struct {
	Name               string `json:"name"`
	IncludeDescendants bool   `json:"includeDescendants,omitempty"`
}

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
	// JudgeRunID is the run that judged the scored run.
	JudgeRunID string `json:"judge_run_id,omitempty"`
	// JudgeUsage is the usage of the judge run. It adds to the cost of the scored run.
	JudgeUsage *Usage `json:"judge_usage,omitempty"`
}

type BenchmarkRun struct {
	ID         string          `json:"id"`
	FactoryID  string          `json:"factory_id"`
	Suite      string          `json:"suite"`
	State      string          `json:"state"` // running, complete, failed, cancelled
	Results    json.RawMessage `json:"results,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	FinishedAt *time.Time      `json:"finished_at,omitempty"`
}

// ---------- Workers ----------

type Worker struct {
	ID            string            `json:"id"`
	TeamID        string            `json:"team_id"`
	Name          string            `json:"name"`
	Backend       string            `json:"backend"` // local, kubernetes, direct
	Labels        map[string]string `json:"labels,omitempty"`
	Capacity      int               `json:"capacity"`
	Active        int               `json:"active"`
	LastHeartbeat time.Time         `json:"last_heartbeat"`
	Version       string            `json:"version,omitempty"`
}

// Environment describes a reusable workspace (repos + setup) agents run in.
type Environment struct {
	ID            string            `json:"id"`
	TeamID        string            `json:"team_id"`
	Name          string            `json:"name"`
	Description   string            `json:"description,omitempty"`
	Repos         []string          `json:"repos"` // forge:owner/name
	Image         string            `json:"image,omitempty"`
	SetupCommands []string          `json:"setup_commands,omitempty"`
	EnvVars       map[string]string `json:"env_vars,omitempty"`
}

// ---------- Inbox resolution ----------

// Tools an inbox item can be resolved from (InboxItem.ResolvedVia).
const (
	InboxViaDashboard = "dashboard"
	InboxViaAPI       = "api"
	InboxViaMCP       = "mcp"
	InboxViaChat      = "chat"
	InboxViaForge     = "forge"
)

// Resolver says who resolves an inbox item and from which tool. Callers of
// Engine.ResolveInbox attach it to the context with WithResolver. The engine
// records it on the item and checks it against the approvers of the factory.
type Resolver struct {
	UserID string // the team member; empty when the sender has no linked account
	Via    string // one of the InboxVia* values
}

type resolverKey struct{}

// WithResolver returns a context that carries r.
func WithResolver(ctx context.Context, r Resolver) context.Context {
	return context.WithValue(ctx, resolverKey{}, r)
}

// ResolverFrom returns the resolver stored in ctx, if any.
func ResolverFrom(ctx context.Context) (Resolver, bool) {
	r, ok := ctx.Value(resolverKey{}).(Resolver)
	return r, ok
}
