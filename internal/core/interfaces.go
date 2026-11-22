package core

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

var (
	ErrNotFound     = rtcore.ErrNotFound
	ErrConflict     = errors.New("conflict")
	ErrInvalid      = rtcore.ErrInvalid
	ErrUnauthorized = rtcore.ErrUnauthorized
	ErrForbidden    = rtcore.ErrForbidden

	// ErrStale wraps ErrConflict. A versioned update (UpdateRun,
	// UpdateWorkItem, UpdateInboxItem) returns it when the stored row changed
	// after the caller read it. The caller reads the row again, applies its
	// change again and retries.
	ErrStale = fmt.Errorf("%w: stored version changed", ErrConflict)

	// ErrIntegrationDisabled means that the integration record that an
	// action needs is not active. A run fails with
	// FailureIntegrationDisabled.
	ErrIntegrationDisabled = errors.New("integration is not active")

	// ErrOperationNotSupported means that a forge or a tracker does not
	// support an operation. The agent gets a tool
	// error with the code operation_not_supported.
	ErrOperationNotSupported = errors.New("operation_not_supported")
)

// ---------- Persistence ----------

type ListOpts struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	// Ascending lists the oldest first. The default is the newest first.
	// The order is (created_at, id) in both cases, so a cursor works only
	// with the order of the request that returned it.
	Ascending bool `json:"ascending,omitempty"`
}

type RunFilter struct {
	TeamID      string
	FactoryID   string
	WorkItemID  string
	ParentRunID string
	AgentName   string
	Name        string
	// CreatorID selects runs whose creator has this principal ID (a user or
	// a service account).
	CreatorID string
	States    []RunState
	Since     *time.Time
	ListOpts
}

type WorkItemFilter struct {
	FactoryID string
	Stages    []Stage
	CreatorID string
	Search    string
	Since     *time.Time
	ListOpts
}

// Store is the persistence contract. Implementations: internal/store/sqlite, internal/store/postgres.
// All Get* methods return ErrNotFound when missing.
type Store interface {
	// Teams / users / auth
	CreateTeam(ctx context.Context, t *Team) error
	GetTeam(ctx context.Context, id string) (*Team, error)
	ListTeams(ctx context.Context) ([]Team, error)
	CreateUser(ctx context.Context, u *User) error
	GetUser(ctx context.Context, id string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	UpdateUser(ctx context.Context, u *User) error
	ListUsers(ctx context.Context, teamID string) ([]User, error)
	UpsertExternalIdentity(ctx context.Context, x *ExternalIdentity) error
	FindExternalIdentity(ctx context.Context, provider, externalID string) (*ExternalIdentity, error)
	CreateAPIKey(ctx context.Context, k *APIKey) error
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (*APIKey, error)
	ListAPIKeys(ctx context.Context, teamID string) ([]APIKey, error)
	DeleteAPIKey(ctx context.Context, id string) error
	TouchAPIKey(ctx context.Context, id string, at time.Time) error

	// Factories & definitions
	CreateFactory(ctx context.Context, f *Factory) error
	UpdateFactory(ctx context.Context, f *Factory) error
	GetFactory(ctx context.Context, id string) (*Factory, error)
	GetFactoryByAlias(ctx context.Context, teamID, alias string) (*Factory, error)
	ListFactories(ctx context.Context, teamID, search string) ([]Factory, error)
	DeleteFactory(ctx context.Context, id string) error
	CreateDefinitionVersion(ctx context.Context, v *DefinitionVersion) error
	UpdateDefinitionVersion(ctx context.Context, v *DefinitionVersion) error
	GetDefinitionVersion(ctx context.Context, id string) (*DefinitionVersion, error)
	ListDefinitionVersions(ctx context.Context, factoryID string, opts ListOpts) ([]DefinitionVersion, error)

	// Materialized resources of the active definition (replaced atomically on apply)
	ReplaceFactoryResources(ctx context.Context, factoryID string, agents []AgentConfig, automations []Automation, scorers []Scorer, webhooks []Webhook) error
	ListAgents(ctx context.Context, factoryID string) ([]AgentConfig, error)
	GetAgent(ctx context.Context, id string) (*AgentConfig, error)
	GetAgentByName(ctx context.Context, factoryID, name string) (*AgentConfig, error)
	ListAutomations(ctx context.Context, factoryID string) ([]Automation, error)
	ListAllEnabledAutomations(ctx context.Context) ([]Automation, error)
	ListScorers(ctx context.Context, factoryID string) ([]Scorer, error)
	ListWebhooks(ctx context.Context, factoryID string) ([]Webhook, error)
	GetWebhook(ctx context.Context, id string) (*Webhook, error)

	// Work items
	// CreateWorkItem stores Version 1. UpdateWorkItem has the same version
	// check as UpdateRun: it writes w only if the stored version equals
	// w.Version, then sets w.Version to the stored version plus 1. A stale
	// write returns an error that wraps ErrStale (and so ErrConflict).
	CreateWorkItem(ctx context.Context, w *WorkItem) error
	UpdateWorkItem(ctx context.Context, w *WorkItem) error
	GetWorkItem(ctx context.Context, id string) (*WorkItem, error)
	FindWorkItemBySource(ctx context.Context, factoryID, sourceKey string) (*WorkItem, error) // most recent non-terminal
	ListWorkItems(ctx context.Context, f WorkItemFilter) ([]WorkItem, error)
	AppendWorkItemEvent(ctx context.Context, e *WorkItemEvent) error
	ListWorkItemEvents(ctx context.Context, workItemID string) ([]WorkItemEvent, error)

	// Runs
	CreateRun(ctx context.Context, r *Run) error
	// UpdateRun writes r only if the stored version equals r.Version. It then
	// stores Version r.Version+1 and sets r.Version to that value. It returns
	// ErrNotFound if the run does not exist, and an error that wraps
	// ErrStale (and so ErrConflict) if the stored version differs. CreateRun
	// stores Version 1.
	// A row from before versions existed has Version 0, and an update with
	// Version 0 succeeds on it.
	UpdateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]Run, error)
	AppendRunMessage(ctx context.Context, m *RunMessage) error // assigns Seq
	ListRunMessages(ctx context.Context, runID string, afterSeq int64) ([]RunMessage, error)

	// Inbox
	// CreateInboxItem stores Version 1. UpdateInboxItem has the version check
	// of UpdateWorkItem.
	CreateInboxItem(ctx context.Context, i *InboxItem) error
	UpdateInboxItem(ctx context.Context, i *InboxItem) error
	GetInboxItem(ctx context.Context, id string) (*InboxItem, error)
	ListInboxItems(ctx context.Context, factoryID string, includeResolved bool) ([]InboxItem, error)

	// Secrets (ciphertext only; encryption lives in internal/secrets)
	PutSecret(ctx context.Context, s *Secret, ciphertext []byte) error
	GetSecret(ctx context.Context, teamID, factoryID, name string) (*Secret, []byte, error) // factory scope falls back to team scope
	ListSecrets(ctx context.Context, teamID, factoryID string) ([]Secret, error)
	DeleteSecret(ctx context.Context, id string) error

	// Webhook deliveries
	CreateWebhookDelivery(ctx context.Context, d *WebhookDelivery) error
	UpdateWebhookDelivery(ctx context.Context, d *WebhookDelivery) error
	FindWebhookDelivery(ctx context.Context, webhookID, deliveryID string) (*WebhookDelivery, error)
	ListWebhookDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error)
	GetWebhookDelivery(ctx context.Context, id string) (*WebhookDelivery, error)

	// Integrations
	UpsertIntegration(ctx context.Context, i *Integration) error
	ListIntegrations(ctx context.Context, teamID string) ([]Integration, error)
	GetIntegration(ctx context.Context, id string) (*Integration, error)
	DeleteIntegration(ctx context.Context, id string) error
	SetFactoryIntegrations(ctx context.Context, factoryID string, providers []string, settings map[string]map[string]string) error
	GetFactoryIntegrations(ctx context.Context, factoryID string) (map[string]map[string]string, error)

	// Scoring
	CreateScore(ctx context.Context, s *Score) error
	UpdateScore(ctx context.Context, s *Score) error
	ListScores(ctx context.Context, factoryID string, scorerID string, since *time.Time) ([]Score, error)
	// GetScore returns the score a scorer gave a run (ErrNotFound when the run was not scored).
	GetScore(ctx context.Context, scorerID, runID string) (*Score, error)
	CreateBenchmarkRun(ctx context.Context, b *BenchmarkRun) error
	UpdateBenchmarkRun(ctx context.Context, b *BenchmarkRun) error
	GetBenchmarkRun(ctx context.Context, id string) (*BenchmarkRun, error)
	ListBenchmarkRuns(ctx context.Context, factoryID string) ([]BenchmarkRun, error)

	// Workers & environments
	UpsertWorker(ctx context.Context, w *Worker) error
	GetWorker(ctx context.Context, id string) (*Worker, error)
	ListWorkers(ctx context.Context, teamID string) ([]Worker, error)
	CreateEnvironment(ctx context.Context, e *Environment) error
	UpdateEnvironment(ctx context.Context, e *Environment) error
	GetEnvironment(ctx context.Context, id string) (*Environment, error)
	ListEnvironments(ctx context.Context, teamID string) ([]Environment, error)
	DeleteEnvironment(ctx context.Context, id string) error

	// MCP server registry. Names are unique per team (ErrConflict).
	CreateMCPServer(ctx context.Context, s *RegisteredMCPServer) error
	UpdateMCPServer(ctx context.Context, s *RegisteredMCPServer) error
	GetMCPServer(ctx context.Context, id string) (*RegisteredMCPServer, error)
	ListMCPServers(ctx context.Context, teamID string) ([]RegisteredMCPServer, error)
	DeleteMCPServer(ctx context.Context, id string) error

	// Generic key/value (cursors, schedule state, misc)
	KVGet(ctx context.Context, key string) (string, error)
	KVSet(ctx context.Context, key, value string) error

	Close() error
}

// ---------- Event bus ----------

// Topics published on the bus.
const (
	TopicInboundEvent   = "event.inbound"      // payload: *Event (from integrations/webhooks/cron)
	TopicRunUpdated     = "run.updated"        // payload: *Run
	TopicRunMessage     = "run.message"        // payload: *RunMessage
	TopicWorkItem       = "workitem.updated"   // payload: *WorkItem
	TopicInbox          = "inbox.updated"      // payload: *InboxItem
	TopicDefinition     = "definition.applied" // payload: *Factory
	TopicOutboundNotice = "notice.outbound"    // payload: *OutboundMessage
)

type EventBus interface {
	Publish(ctx context.Context, topic string, payload any)
	// Subscribe returns a channel and an unsubscribe func. Slow subscribers may drop messages.
	Subscribe(topic string) (<-chan any, func())
}

// OutboundMessage is something the factory says back to a work source.
type OutboundMessage struct {
	WorkItemID string    `json:"work_item_id"`
	Source     SourceRef `json:"source"`
	Text       string    `json:"text"` // markdown
	Kind       string    `json:"kind"` // update, question, approval_request, handoff, error
	Options    []string  `json:"options,omitempty"`
	// InboxID is set when the message asks a human for a decision recorded in the inbox
	// (approval_request, question); responders use it for action buttons.
	InboxID   string `json:"inbox_id,omitempty"`
	FactoryID string `json:"factory_id,omitempty"`
	// ThreadRef is the sub-thread of the source that the reply goes into,
	// for example a review thread. Empty means a
	// top-level reply.
	ThreadRef string `json:"thread_ref,omitempty"`
	// IssueState is the state to set for Kind OutboundIssueState
	// (IssueStateInProgress, IssueStateInReview or IssueStateDone).
	IssueState string `json:"issue_state,omitempty"`
	// PullRequest is the pull request to attach for Kind
	// OutboundAttachPullRequest.
	PullRequest *PullRequestRef `json:"pull_request,omitempty"`
}

// ---------- Harness ----------

// Outcomes of a message to a run: where it went.
const (
	// MessageLiveSession means that a live session got the message.
	MessageLiveSession = "live_session"
	// MessageNewSession means that the message starts a new session.
	MessageNewSession = "new_session"
	// MessageQueued means that the run already waits for a session, or a turn is active.
	MessageQueued = "queued"
)

// MessageReceipt is the record that the engine returns for a message to a
// run: the ID of the stored user message (the ID of its queue entry on the
// runner path) and the outcome. The outcome is one of MessageLiveSession,
// MessageNewSession and MessageQueued.
type MessageReceipt struct {
	MessageID string `json:"message_id"`
	Outcome   string `json:"outcome"`
}

// ---------- Sandboxes ----------

type SandboxSpec struct {
	RunID         string
	Machine       MachineSpec
	Environment   *Environment
	Repos         []RepoCheckout
	Env           map[string]string
	NetworkPolicy EffectiveNetworkPolicy
}

type RepoCheckout struct {
	Forge    string // GITHUB, GITLAB, AZURE_DEVOPS, GIT
	Owner    string
	Name     string
	CloneURL string // with credentials embedded or via Env
	Ref      string // branch or commit; empty = default branch
	Path     string // relative destination inside the workspace
}

// ---------- Forges ----------

type Forge interface {
	Name() string                                                     // GITHUB, GITLAB, AZURE_DEVOPS
	CloneURL(ctx context.Context, owner, repo string) (string, error) // authenticated
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	CreatePullRequest(ctx context.Context, req PullRequestInput) (*PullRequestRef, error)
	GetPullRequest(ctx context.Context, owner, repo string, number int) (*PullRequestRef, error)
	Comment(ctx context.Context, owner, repo string, number int, body string) error
	SetCommitStatus(ctx context.Context, owner, repo, sha string, st CommitStatus) error
	// GetFiles reads a tree at ref (for git-backed definitions).
	GetFiles(ctx context.Context, owner, repo, ref, path string) (map[string]string, string, error) // files, commit sha

	// The review operations. A forge that does not
	// support one returns an error that wraps ErrOperationNotSupported.

	// ReplyInThread replies in a review thread of a pull request. threadRef
	// is SourceRef.ThreadRef of the event: the ID of the first comment of
	// the thread (GitHub) or the discussion or thread ID (GitLab, Azure DevOps).
	ReplyInThread(ctx context.Context, owner, repo string, number int, threadRef, body string) error
	// ResolveThread marks a review thread as resolved.
	ResolveThread(ctx context.Context, owner, repo string, number int, threadRef string) error
	// SubmitReview submits a review with inline comments.
	SubmitReview(ctx context.Context, in ReviewInput) error
	// MarkReady marks a draft pull request as ready for review.
	MarkReady(ctx context.Context, owner, repo string, number int) error
}

// Review events of ReviewInput.Event.
const (
	ReviewEventComment        = "comment"
	ReviewEventApprove        = "approve"
	ReviewEventRequestChanges = "request_changes"
)

// ReviewInput is the input of Forge.SubmitReview.
type ReviewInput struct {
	Owner, Repo string
	Number      int
	Event       string // ReviewEventComment (default), ReviewEventApprove or ReviewEventRequestChanges
	Body        string
	Comments    []ReviewComment
}

// ReviewComment is one inline comment of a review.
type ReviewComment struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	// Side is "RIGHT" (the new file, the default) or "LEFT" (the old file).
	Side string `json:"side,omitempty"`
	Body string `json:"body"`
}

type PullRequestInput struct {
	Owner, Repo, Head, Base, Title, Body string
	Draft                                bool
}

type CommitStatus struct {
	Context     string
	State       string // pending, success, failure, error
	Description string
	TargetURL   string
}

// ---------- Chat / trackers ----------

// Responder posts outbound messages back to a work source (Slack thread, Linear issue, GitHub issue...).
type Responder interface {
	Provider() string
	Reply(ctx context.Context, teamID string, msg OutboundMessage) error
}

// IssueTracker is the write-back of a tracker responder. The notify router
// calls it for the outbound kinds OutboundIssueState
// and OutboundAttachPullRequest. A responder without it, or one that returns
// ErrOperationNotSupported, skips the operation.
type IssueTracker interface {
	// SetIssueState moves the issue to the tracker state that matches state
	// (IssueStateInProgress, IssueStateInReview or IssueStateDone).
	SetIssueState(ctx context.Context, teamID string, issue SourceRef, state string) error
	// AttachPullRequest attaches a pull request link to the issue.
	AttachPullRequest(ctx context.Context, teamID string, issue SourceRef, pr PullRequestRef) error
}

// ---------- Secrets ----------

type SecretStore interface {
	Put(ctx context.Context, teamID, factoryID, name string, typ SecretType, value []byte) (*Secret, error)
	Get(ctx context.Context, teamID, factoryID, name string) ([]byte, error)
	List(ctx context.Context, teamID, factoryID string) ([]Secret, error)
	Delete(ctx context.Context, id string) error
	// Expand replaces {{SECRET_NAME}} placeholders.
	Expand(ctx context.Context, teamID, factoryID, s string) (string, error)
}

// ---------- Engine (used by api, mcpserver, orchestration, integrations) ----------

type DispatchRequest struct {
	FactoryID    string
	Prompt       string
	Source       SourceRef
	Event        *Event
	AutomationID string
	AgentName    string // default foreman
	CreatorID    string
	Title        string
	Overrides    *AgentOverrides
	Metadata     map[string]string
	TriggeredBy  string
	// StartingRepoRefs pins repositories to exact commits for every run of the work
	// item (benchmark trials).
	StartingRepoRefs []RepoAtCommit
	// AgentPins are overrides applied when a run of the work item starts the named agent.
	AgentPins map[string]AgentOverrides
}

type ChildRunConfig struct {
	Name             string            `json:"name"`
	AgentIdentityUID string            `json:"agent_identity_uid"`
	Prompt           string            `json:"prompt"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Overrides        *AgentOverrides   `json:"overrides,omitempty"`
	// Skills attaches skills to the child: factory skill names, skill
	// names of other agents, built-in skill names or skill specs
	// (owner/repo:path).
	Skills []string `json:"skills,omitempty"`
}

type RunAgentsRequest struct {
	ParentRunID     string            `json:"-"`
	Summary         string            `json:"summary"`
	BasePrompt      string            `json:"base_prompt"`
	Remote          map[string]any    `json:"remote,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`
	AgentRunConfigs []ChildRunConfig  `json:"agent_run_configs"`
}

// IssueLinker is implemented by the engine (internal/engine). It is apart
// from Engine so that test fakes of Engine do not need it.
type IssueLinker interface {
	// LinkIssue adds a tracker issue to the references of a work item.
	// Events on the issue route to the work item, and the engine writes the
	// state of the work item back to the issue.
	LinkIssue(ctx context.Context, workItemID string, issue SourceRef) error
}

// Engine is the orchestration core (implemented in internal/engine).
type Engine interface {
	// HandleEvent routes a normalized inbound event (follow-up on existing work item, or matched automations).
	HandleEvent(ctx context.Context, ev *Event) ([]*Run, error)
	// Dispatch creates (or reuses) a work item and starts a run for it.
	Dispatch(ctx context.Context, req DispatchRequest) (*WorkItem, *Run, error)
	RunAgents(ctx context.Context, req RunAgentsRequest) ([]*Run, error)
	FollowUp(ctx context.Context, runID, message, actor string) error
	// SendMessage is FollowUp with attachments (IDs of core.Attachment). Attachments need the runner execution path (ErrInvalid otherwise).
	// It returns the record of the message: its ID and where it went.
	SendMessage(ctx context.Context, runID, message, actor string, attachments []string) (MessageReceipt, error)
	Cancel(ctx context.Context, runID, actor string) error
	// CancelOne cancels one run and not the runs under it (cascade false).
	CancelOne(ctx context.Context, runID, actor string) error
	// Interrupt stops the active turn of a run and keeps its session. It returns *InterruptError if it cannot.
	Interrupt(ctx context.Context, runID string) error
	SetStage(ctx context.Context, workItemID string, stage Stage, actor, note string) error
	RequestHuman(ctx context.Context, runID string, kind InboxKind, title, body string, options []string) (*InboxItem, error)
	ResolveInbox(ctx context.Context, inboxID, resolution, actor string) error
	// UnresolveInbox undoes a resolve within a short time, if the run did not consume the answer yet (ErrConflict otherwise).
	UnresolveInbox(ctx context.Context, inboxID, actor string) (*InboxItem, error)
	PostUpdate(ctx context.Context, runID, text, kind string) error
	LinkPullRequest(ctx context.Context, workItemID string, pr PullRequestRef) error
	CompleteWorkItem(ctx context.Context, workItemID, actor, note string) error
	ApplyDefinition(ctx context.Context, factoryID string, files map[string]string, author, message, commitSHA string) (*DefinitionVersion, error)
}
