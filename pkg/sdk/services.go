package sdk

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func setIf(q url.Values, k, v string) {
	if v != "" {
		q.Set(k, v)
	}
}

func timeParam(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ---------- factories ----------

// FactoriesService wraps /factories endpoints.
type FactoriesService struct{ c *Client }

// FactoryListOptions filters factory listings.
type FactoryListOptions struct {
	Search   string // case-insensitive name/alias filter
	PageSize int
}

// List iterates factories visible to the caller.
func (s *FactoriesService) List(_ context.Context, opts *FactoryListOptions) *Iter[Factory] {
	q := url.Values{}
	size := 0
	if opts != nil {
		setIf(q, "search", opts.Search)
		size = opts.PageSize
	}
	return listIter[Factory](s.c, "factories", "factories", q, size)
}

// Get fetches one factory.
func (s *FactoriesService) Get(ctx context.Context, uid string) (*Factory, error) {
	var f Factory
	return &f, s.c.Do(ctx, "GET", "factories/"+pathEscape(uid), nil, nil, &f)
}

// CreateFactoryParams is the body of a factory creation.
type CreateFactoryParams struct {
	Name         string            `json:"name,omitempty"`
	Alias        string            `json:"alias,omitempty"`
	Description  string            `json:"description,omitempty"`
	Source       string            `json:"source,omitempty"` // managed | git
	GitRepo      string            `json:"git_repo,omitempty"`
	GitBranch    string            `json:"git_branch,omitempty"`
	GitPath      string            `json:"git_path,omitempty"`
	Repositories []RepoRef         `json:"repositories,omitempty"`
	Integrations []IntegrationDecl `json:"integrations,omitempty"`
	Agents       *[]string         `json:"agents,omitempty"` // nil = all default agents
	Model        string            `json:"model,omitempty"`
	Harness      *HarnessConfig    `json:"harness,omitempty"`
	Files        map[string]string `json:"files,omitempty"` // full definition tree instead of defaults
}

// FactoryCreated is the result of a creation.
type FactoryCreated struct {
	Factory Factory            `json:"factory"`
	Version *DefinitionVersion `json:"version,omitempty"`
}

// Create creates a factory (requires the editor role).
func (s *FactoriesService) Create(ctx context.Context, p CreateFactoryParams) (*FactoryCreated, error) {
	var out FactoryCreated
	return &out, s.c.Do(ctx, "POST", "factories", nil, p, &out)
}

// UpdateFactoryParams patches a factory; nil fields are unchanged.
type UpdateFactoryParams struct {
	Name        *string `json:"name,omitempty"`
	Alias       *string `json:"alias,omitempty"`
	Description *string `json:"description,omitempty"`
	GitRepo     *string `json:"git_repo,omitempty"`
	GitBranch   *string `json:"git_branch,omitempty"`
	GitPath     *string `json:"git_path,omitempty"`
}

// Update patches a factory.
func (s *FactoriesService) Update(ctx context.Context, uid string, p UpdateFactoryParams) (*Factory, error) {
	var f Factory
	return &f, s.c.Do(ctx, "PATCH", "factories/"+pathEscape(uid), nil, p, &f)
}

// Delete deletes a factory (requires the admin role).
func (s *FactoriesService) Delete(ctx context.Context, uid string) error {
	return s.c.Do(ctx, "DELETE", "factories/"+pathEscape(uid), nil, nil, nil)
}

// DispatchParams is the body of a dispatch to a factory's foreman.
type DispatchParams struct {
	Prompt    string            `json:"prompt"`
	Title     string            `json:"title,omitempty"`
	TicketRef string            `json:"ticket_ref,omitempty"` // <source>:<id>, e.g. linear:PAY-123
	TicketURL string            `json:"ticket_url,omitempty"`
	Source    *SourceRef        `json:"source,omitempty"`
	Agent     string            `json:"agent,omitempty"`
	Metadata  map[string]string `json:"metadata,omitempty"`
}

// Dispatch starts a run on the factory's foreman.
func (s *FactoriesService) Dispatch(ctx context.Context, uid string, p DispatchParams) (*Run, error) {
	var r Run
	return &r, s.c.Do(ctx, "POST", "factories/"+pathEscape(uid)+"/runs", nil, p, &r)
}

// Agents lists a factory's agents.
func (s *FactoriesService) Agents(ctx context.Context, uid string) ([]Agent, error) {
	return listAll[Agent](ctx, s.c, "factories/"+pathEscape(uid)+"/agents", "agents", nil)
}

// Agent fetches one agent by name or ID.
func (s *FactoriesService) Agent(ctx context.Context, uid, name string) (*Agent, error) {
	var a Agent
	return &a, s.c.Do(ctx, "GET", "factories/"+pathEscape(uid)+"/agents/"+pathEscape(name), nil, nil, &a)
}

// Automations lists a factory's automations.
func (s *FactoriesService) Automations(ctx context.Context, uid string) ([]Automation, error) {
	return listAll[Automation](ctx, s.c, "factories/"+pathEscape(uid)+"/automations", "automations", nil)
}

// ---------- definitions ----------

// DefinitionsService wraps /factories/{uid}/definition endpoints.
type DefinitionsService struct{ c *Client }

// Get returns the active definition.
func (s *DefinitionsService) Get(ctx context.Context, uid string) (*Definition, error) {
	var d Definition
	return &d, s.c.Do(ctx, "GET", "factories/"+pathEscape(uid)+"/definition", nil, nil, &d)
}

// PutDefinitionParams replaces a managed factory's definition.
type PutDefinitionParams struct {
	Files         map[string]string `json:"files"`
	Message       string            `json:"message,omitempty"`
	BaseVersionID string            `json:"base_version_id,omitempty"` // optimistic concurrency
}

// Put validates and applies a new definition.
func (s *DefinitionsService) Put(ctx context.Context, uid string, p PutDefinitionParams) (*DefinitionVersion, error) {
	var out struct {
		Version DefinitionVersion `json:"version"`
	}
	return &out.Version, s.c.Do(ctx, "PUT", "factories/"+pathEscape(uid)+"/definition", nil, p, &out)
}

// Validate validates a definition tree without applying it.
func (s *DefinitionsService) Validate(ctx context.Context, uid string, files map[string]string) (*ValidationResult, error) {
	var out ValidationResult
	return &out, s.c.Do(ctx, "POST", "factories/"+pathEscape(uid)+"/definition/validate", nil, map[string]any{"files": files}, &out)
}

// Versions iterates definition versions (files omitted unless includeFiles).
func (s *DefinitionsService) Versions(_ context.Context, uid string, includeFiles bool) *Iter[DefinitionVersion] {
	q := url.Values{}
	if includeFiles {
		q.Set("include_files", "true")
	}
	return listIter[DefinitionVersion](s.c, "factories/"+pathEscape(uid)+"/definition/versions", "versions", q, 0)
}

// Version fetches one definition version.
func (s *DefinitionsService) Version(ctx context.Context, uid, versionID string) (*DefinitionVersion, error) {
	var v DefinitionVersion
	return &v, s.c.Do(ctx, "GET", "factories/"+pathEscape(uid)+"/definition/versions/"+pathEscape(versionID), nil, nil, &v)
}

// ---------- runs ----------

// RunsService wraps /runs endpoints.
type RunsService struct{ c *Client }

// RunConfig holds optional overrides for the agent of a run.
type RunConfig struct {
	Harness            *HarnessConfig       `json:"harness,omitempty"`
	ModelID            string               `json:"model_id,omitempty"`
	EnvironmentID      string               `json:"environment_id,omitempty"`
	Machine            string               `json:"machine,omitempty"`
	WorkerHost         string               `json:"worker_host,omitempty"`
	MCPServers         map[string]MCPServer `json:"mcp_servers,omitempty"`
	Secrets            []string             `json:"secrets,omitempty"`
	IdleTimeoutMinutes int                  `json:"idle_timeout_minutes,omitempty"`
}

// CreateRunParams starts a run of an agent of a factory. Set
// AgentIdentityUID, or FactoryID and AgentName.
type CreateRunParams struct {
	Prompt           string            `json:"prompt"`
	Title            string            `json:"title,omitempty"`
	AgentIdentityUID string            `json:"agent_identity_uid,omitempty"`
	FactoryID        string            `json:"factory_id,omitempty"`
	AgentName        string            `json:"agent_name,omitempty"`
	Config           *RunConfig        `json:"config,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Create starts a run.
func (s *RunsService) Create(ctx context.Context, p CreateRunParams) (*Run, error) {
	var r Run
	return &r, s.c.Do(ctx, "POST", "runs", nil, p, &r)
}

// RunListOptions filters run listings.
type RunListOptions struct {
	Name        string   // config name
	States      []string // QUEUED, PENDING, CLAIMED, RUNNING, BLOCKED, SUCCEEDED, FAILED, ERROR, CANCELLED
	FactoryID   string
	WorkItemID  string
	ParentRunID string
	AgentName   string
	// Creator is a principal ID or "me".
	Creator string
	// Sources are values of Run.Source.
	Sources []string
	Models  []string
	Since   *time.Time
	// Ascending lists the oldest first.
	Ascending bool
	PageSize  int
}

// List iterates runs.
func (s *RunsService) List(_ context.Context, opts *RunListOptions) *Iter[Run] {
	q := url.Values{}
	size := 0
	if opts != nil {
		setIf(q, "name", opts.Name)
		setIf(q, "state", strings.Join(opts.States, ","))
		setIf(q, "factory_id", opts.FactoryID)
		setIf(q, "work_item_id", opts.WorkItemID)
		setIf(q, "parent_run_id", opts.ParentRunID)
		setIf(q, "agent_name", opts.AgentName)
		setIf(q, "creator", opts.Creator)
		setIf(q, "source", strings.Join(opts.Sources, ","))
		setIf(q, "model", strings.Join(opts.Models, ","))
		setIf(q, "since", timeParam(opts.Since))
		if opts.Ascending {
			q.Set("sort_order", "asc")
		}
		size = opts.PageSize
	}
	return listIter[Run](s.c, "runs", "runs", q, size)
}

// Get fetches a run.
func (s *RunsService) Get(ctx context.Context, runID string) (*Run, error) {
	var r Run
	return &r, s.c.Do(ctx, "GET", "runs/"+pathEscape(runID), nil, nil, &r)
}

// Messages returns all transcript messages with seq > after.
func (s *RunsService) Messages(ctx context.Context, runID string, after int64) ([]RunMessage, error) {
	var all []RunMessage
	for {
		q := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {"1000"}}
		var page struct {
			Messages  []RunMessage `json:"messages"`
			HasMore   bool         `json:"has_more"`
			NextAfter int64        `json:"next_after"`
		}
		if err := s.c.Do(ctx, "GET", "runs/"+pathEscape(runID)+"/messages", q, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Messages...)
		if !page.HasMore || page.NextAfter <= after {
			return all, nil
		}
		after = page.NextAfter
	}
}

// FollowUp sends a follow-up message to a run.
func (s *RunsService) FollowUp(ctx context.Context, runID, message string) (*Run, error) {
	var r Run
	return &r, s.c.Do(ctx, "POST", "runs/"+pathEscape(runID)+"/followups", nil, map[string]string{"message": message}, &r)
}

// Cancel cancels a run and the runs under it.
func (s *RunsService) Cancel(ctx context.Context, runID string) error {
	return s.CancelRun(ctx, runID, true)
}

// CancelRun cancels a run. When cascade is true, it also cancels the runs
// under the run.
func (s *RunsService) CancelRun(ctx context.Context, runID string, cascade bool) error {
	return s.c.Do(ctx, "POST", "runs/"+pathEscape(runID)+"/cancel", nil, map[string]bool{"cascade": cascade}, nil)
}

// SendMessage sends a message to a run in any state.
func (s *RunsService) SendMessage(ctx context.Context, runID string, p SendMessageParams) (*SendMessageResult, error) {
	body := map[string]any{"message": p.Message}
	if len(p.Attachments) > 0 {
		body["attachments"] = p.Attachments
	}
	var out SendMessageResult
	return &out, s.c.Do(ctx, "POST", "runs/"+pathEscape(runID)+"/messages", nil, body, &out)
}

// Interrupt stops the active turn of a run and keeps its session. A run
// that cannot be interrupted gives a conflict error (see IsConflict).
func (s *RunsService) Interrupt(ctx context.Context, runID string) error {
	return s.c.Do(ctx, "POST", "runs/"+pathEscape(runID)+"/interrupt", nil, nil, nil)
}

// Timeline returns the timeline events of a run.
func (s *RunsService) Timeline(ctx context.Context, runID string) ([]TimelineEvent, error) {
	var out ListPage[TimelineEvent]
	err := s.c.Do(ctx, "GET", "runs/"+pathEscape(runID)+"/timeline", nil, nil, &out)
	return out.Items, err
}

// Diagnostics returns the diagnostics of a run.
func (s *RunsService) Diagnostics(ctx context.Context, runID string) ([]Diagnostic, error) {
	var out ListPage[Diagnostic]
	err := s.c.Do(ctx, "GET", "runs/"+pathEscape(runID)+"/diagnostics", nil, nil, &out)
	return out.Items, err
}

// Sessions returns the sessions of a run.
func (s *RunsService) Sessions(ctx context.Context, runID string) ([]RunSession, error) {
	var out ListPage[RunSession]
	err := s.c.Do(ctx, "GET", "runs/"+pathEscape(runID)+"/sessions", nil, nil, &out)
	return out.Items, err
}

// Inputs returns the message queue of a run.
func (s *RunsService) Inputs(ctx context.Context, runID string) ([]RunInput, error) {
	var out ListPage[RunInput]
	err := s.c.Do(ctx, "GET", "runs/"+pathEscape(runID)+"/inputs", nil, nil, &out)
	return out.Items, err
}

// ConversationOptions selects what Conversation returns.
type ConversationOptions struct {
	// FinalMessageOnly returns only the last assistant message
	// (scope=final_message).
	FinalMessageOnly bool
}

// Conversation returns the normalized conversation of a run. The options
// can be nil.
func (s *RunsService) Conversation(ctx context.Context, runID string, opts *ConversationOptions) (*Conversation, error) {
	q := url.Values{}
	if opts != nil && opts.FinalMessageOnly {
		q.Set("scope", "final_message")
	}
	var out Conversation
	return &out, s.c.Do(ctx, "GET", "runs/"+pathEscape(runID)+"/conversation", q, nil, &out)
}

// UploadAttachment uploads a file. Use the ID of the result in
// SendMessageParams.Attachments.
func (s *RunsService) UploadAttachment(ctx context.Context, name, contentType string, r io.Reader) (*Attachment, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("openfactory: read attachment: %w", err)
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	res, err := s.c.rawRequest(ctx, "POST", "attachments", url.Values{"name": {name}}, data, "application/json", map[string]string{"Content-Type": contentType})
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out Attachment
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("openfactory: decode response: %w", err)
	}
	return &out, nil
}

// Wait polls a run until it is finished and returns it. When untilIdle is
// true, it also returns when the run is BLOCKED, so the turn is done and the
// run waits for a person. The name stays for compatibility.
func (s *RunsService) Wait(ctx context.Context, runID string, interval time.Duration, untilIdle bool) (*Run, error) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	for {
		r, err := s.Get(ctx, runID)
		if err != nil {
			return nil, err
		}
		if r.Finished() || (untilIdle && r.WaitsForInput()) {
			return r, nil
		}
		if err := sleep(ctx, interval); err != nil {
			return nil, err
		}
	}
}

// ---------- work items ----------

// WorkItemsService wraps work item endpoints.
type WorkItemsService struct{ c *Client }

// WorkItemListOptions filters work items.
type WorkItemListOptions struct {
	Stages   []string
	Creator  string // user ID or "me"
	Search   string
	Since    *time.Time
	PageSize int
}

// List iterates a factory's work items.
func (s *WorkItemsService) List(_ context.Context, factoryUID string, opts *WorkItemListOptions) *Iter[WorkItem] {
	q := url.Values{}
	size := 0
	if opts != nil {
		setIf(q, "stage", strings.Join(opts.Stages, ","))
		setIf(q, "creator", opts.Creator)
		setIf(q, "search", opts.Search)
		setIf(q, "since", timeParam(opts.Since))
		size = opts.PageSize
	}
	return listIter[WorkItem](s.c, "factories/"+pathEscape(factoryUID)+"/work-items", "work_items", q, size)
}

// Get fetches a work item with its timeline and runs.
func (s *WorkItemsService) Get(ctx context.Context, id string) (*WorkItemDetail, error) {
	var d WorkItemDetail
	return &d, s.c.Do(ctx, "GET", "work-items/"+pathEscape(id), nil, nil, &d)
}

// Message sends a message to the work item's foreman.
func (s *WorkItemsService) Message(ctx context.Context, id, message string) error {
	return s.c.Do(ctx, "POST", "work-items/"+pathEscape(id)+"/messages", nil, map[string]string{"message": message}, nil)
}

// SendMessage is Message with the record of the message: its ID and where it
// went (live_session, queued or new_session).
func (s *WorkItemsService) SendMessage(ctx context.Context, id, message string) (*SendMessageResult, error) {
	var out SendMessageResult
	if err := s.c.Do(ctx, "POST", "work-items/"+pathEscape(id)+"/messages", nil, map[string]string{"message": message}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cancel cancels a work item.
func (s *WorkItemsService) Cancel(ctx context.Context, id, reason string) (*WorkItem, error) {
	var w WorkItem
	return &w, s.c.Do(ctx, "POST", "work-items/"+pathEscape(id)+"/cancel", nil, map[string]string{"reason": reason}, &w)
}

// Complete marks a work item complete.
func (s *WorkItemsService) Complete(ctx context.Context, id, note string) (*WorkItem, error) {
	var w WorkItem
	return &w, s.c.Do(ctx, "POST", "work-items/"+pathEscape(id)+"/complete", nil, map[string]string{"note": note}, &w)
}

// SetStage moves a work item to a stage (requires the editor role).
func (s *WorkItemsService) SetStage(ctx context.Context, id, stage, note string) (*WorkItem, error) {
	var w WorkItem
	return &w, s.c.Do(ctx, "POST", "work-items/"+pathEscape(id)+"/stage", nil, map[string]string{"stage": stage, "note": note}, &w)
}

// ---------- inbox ----------

// InboxService wraps inbox endpoints.
type InboxService struct{ c *Client }

// InboxListOptions filters inbox listings.
type InboxListOptions struct {
	FactoryID       string
	Kinds           []string
	IncludeResolved bool
	// Team lists all items (scope=team), for an editor or an admin. The
	// default lists the items for the caller.
	Team     bool
	Unread   bool
	Search   string
	PageSize int
}

// List iterates inbox items across the team's factories.
func (s *InboxService) List(_ context.Context, opts *InboxListOptions) *Iter[InboxItem] {
	q := url.Values{}
	size := 0
	if opts != nil {
		setIf(q, "factory_id", opts.FactoryID)
		setIf(q, "kind", strings.Join(opts.Kinds, ","))
		setIf(q, "search", opts.Search)
		if opts.IncludeResolved {
			q.Set("include_resolved", "true")
		}
		if opts.Team {
			q.Set("scope", "team")
		}
		if opts.Unread {
			q.Set("unread", "true")
		}
		size = opts.PageSize
	}
	return listIter[InboxItem](s.c, "inbox", "inbox_items", q, size)
}

// Get fetches an inbox item.
func (s *InboxService) Get(ctx context.Context, id string) (*InboxItem, error) {
	var it InboxItem
	return &it, s.c.Do(ctx, "GET", "inbox/"+pathEscape(id), nil, nil, &it)
}

// Resolve resolves an inbox item (approval option, answer, or acknowledgement).
func (s *InboxService) Resolve(ctx context.Context, id, resolution string) (*InboxItem, error) {
	var it InboxItem
	return &it, s.c.Do(ctx, "POST", "inbox/"+pathEscape(id)+"/resolve", nil, map[string]string{"resolution": resolution}, &it)
}

// Answer resolves a question item with answers to its questions.
func (s *InboxService) Answer(ctx context.Context, id string, answers []InboxAnswer) (*InboxItem, error) {
	var it InboxItem
	return &it, s.c.Do(ctx, "POST", "inbox/"+pathEscape(id)+"/resolve", nil, map[string]any{"answers": answers}, &it)
}

// MarkRead sets the read state of an item for the caller.
func (s *InboxService) MarkRead(ctx context.Context, id string, read bool) (*InboxItem, error) {
	var it InboxItem
	return &it, s.c.Do(ctx, "POST", "inbox/"+pathEscape(id)+"/read", nil, map[string]bool{"read": read}, &it)
}

// Unresolve undoes a resolve. It works for 10 minutes, and only while the
// run did not read the answer (CodeConflict otherwise).
func (s *InboxService) Unresolve(ctx context.Context, id string) (*InboxItem, error) {
	var it InboxItem
	return &it, s.c.Do(ctx, "POST", "inbox/"+pathEscape(id)+"/unresolve", nil, nil, &it)
}

// ---------- webhooks ----------

// WebhooksService wraps webhook management endpoints.
type WebhooksService struct{ c *Client }

// CreateWebhookParams creates a webhook on a managed factory.
type CreateWebhookParams struct {
	Name             string `json:"name"`
	AuthMode         string `json:"auth_mode,omitempty"` // token | url_token | signature
	SignatureScheme  string `json:"signature_scheme,omitempty"`
	SecretName       string `json:"secret_name,omitempty"`
	Secret           string `json:"secret,omitempty"`
	DeliveryIDHeader string `json:"delivery_id_header,omitempty"`
	Enabled          *bool  `json:"enabled,omitempty"`
}

// List lists a factory's webhooks.
func (s *WebhooksService) List(ctx context.Context, factoryUID string) ([]Webhook, error) {
	return listAll[Webhook](ctx, s.c, "factories/"+pathEscape(factoryUID)+"/webhooks", "webhooks", nil)
}

// Create creates a webhook; the returned secret is shown once.
func (s *WebhooksService) Create(ctx context.Context, factoryUID string, p CreateWebhookParams) (*WebhookCreated, error) {
	var out WebhookCreated
	return &out, s.c.Do(ctx, "POST", "factories/"+pathEscape(factoryUID)+"/webhooks", nil, p, &out)
}

// Get fetches a webhook.
func (s *WebhooksService) Get(ctx context.Context, id string) (*Webhook, error) {
	var w Webhook
	return &w, s.c.Do(ctx, "GET", "webhooks/"+pathEscape(id), nil, nil, &w)
}

// UpdateWebhookParams patches a webhook.
type UpdateWebhookParams struct {
	Enabled          *bool   `json:"enabled,omitempty"`
	DeliveryIDHeader *string `json:"delivery_id_header,omitempty"`
	Secret           *string `json:"secret,omitempty"` // provider signing secret
}

// Update patches a webhook.
func (s *WebhooksService) Update(ctx context.Context, id string, p UpdateWebhookParams) (*Webhook, error) {
	var w Webhook
	return &w, s.c.Do(ctx, "PATCH", "webhooks/"+pathEscape(id), nil, p, &w)
}

// Delete deletes a webhook.
func (s *WebhooksService) Delete(ctx context.Context, id string) error {
	return s.c.Do(ctx, "DELETE", "webhooks/"+pathEscape(id), nil, nil, nil)
}

// RotateSecret rotates a webhook's secret and returns the new one (empty for provider-supplied secrets).
func (s *WebhooksService) RotateSecret(ctx context.Context, id, providerSecret string) (string, error) {
	var out struct {
		Secret string `json:"secret"`
	}
	var body any
	if providerSecret != "" {
		body = map[string]string{"secret": providerSecret}
	}
	return out.Secret, s.c.Do(ctx, "POST", "webhooks/"+pathEscape(id)+"/rotate-secret", nil, body, &out)
}

// Deliveries lists recent deliveries, newest first.
func (s *WebhooksService) Deliveries(ctx context.Context, id string, limit int) ([]Delivery, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return listAll[Delivery](ctx, s.c, "webhooks/"+pathEscape(id)+"/deliveries", "deliveries", q)
}

// Delivery fetches one delivery including its payload.
func (s *WebhooksService) Delivery(ctx context.Context, id, deliveryID string) (*Delivery, error) {
	var d Delivery
	return &d, s.c.Do(ctx, "GET", "webhooks/"+pathEscape(id)+"/deliveries/"+pathEscape(deliveryID), nil, nil, &d)
}

// TestFilterParams evaluates a trigger filter against a delivery.
type TestFilterParams struct {
	Filter     json.RawMessage `json:"filter,omitempty"`
	Pattern    json.RawMessage `json:"pattern,omitempty"`
	DeliveryID string          `json:"delivery_id,omitempty"`
	Body       json.RawMessage `json:"body,omitempty"`
}

// TestFilter reports whether a delivery would trigger an automation.
func (s *WebhooksService) TestFilter(ctx context.Context, id string, p TestFilterParams) (*TestFilterResult, error) {
	var out TestFilterResult
	return &out, s.c.Do(ctx, "POST", "webhooks/"+pathEscape(id)+"/test-filter", nil, p, &out)
}

// ---------- secrets ----------

// SecretsService wraps secret endpoints. Values are write-only.
type SecretsService struct{ c *Client }

func scopeQuery(factoryID string) url.Values {
	q := url.Values{}
	setIf(q, "factory_id", factoryID)
	return q
}

// List lists secrets (team scope, plus a factory's scope when factoryID is set).
func (s *SecretsService) List(ctx context.Context, factoryID string) ([]Secret, error) {
	return listAll[Secret](ctx, s.c, "secrets", "secrets", scopeQuery(factoryID))
}

// Create creates a secret.
func (s *SecretsService) Create(ctx context.Context, name, typ, value, factoryID string) (*Secret, error) {
	var out Secret
	body := map[string]string{"name": name, "type": typ, "value": value, "factory_id": factoryID}
	return &out, s.c.Do(ctx, "POST", "secrets", nil, body, &out)
}

// Get returns secret metadata.
func (s *SecretsService) Get(ctx context.Context, name, factoryID string) (*Secret, error) {
	var out Secret
	return &out, s.c.Do(ctx, "GET", "secrets/"+pathEscape(name), scopeQuery(factoryID), nil, &out)
}

// Update replaces a secret's value.
func (s *SecretsService) Update(ctx context.Context, name, value, factoryID string) (*Secret, error) {
	var out Secret
	return &out, s.c.Do(ctx, "PUT", "secrets/"+pathEscape(name), scopeQuery(factoryID), map[string]string{"value": value}, &out)
}

// Delete deletes a secret.
func (s *SecretsService) Delete(ctx context.Context, name, factoryID string) error {
	return s.c.Do(ctx, "DELETE", "secrets/"+pathEscape(name), scopeQuery(factoryID), nil, nil)
}

// ---------- environments ----------

// EnvironmentsService wraps environment endpoints.
type EnvironmentsService struct{ c *Client }

// EnvironmentParams creates or patches an environment.
type EnvironmentParams struct {
	Name          *string           `json:"name,omitempty"`
	Description   *string           `json:"description,omitempty"`
	Repos         []string          `json:"repos,omitempty"`
	Image         *string           `json:"image,omitempty"`
	SetupCommands []string          `json:"setup_commands,omitempty"`
	EnvVars       map[string]string `json:"env_vars,omitempty"`
}

// List lists environments.
func (s *EnvironmentsService) List(ctx context.Context) ([]Environment, error) {
	return listAll[Environment](ctx, s.c, "environments", "environments", nil)
}

// Create creates an environment.
func (s *EnvironmentsService) Create(ctx context.Context, p EnvironmentParams) (*Environment, error) {
	var e Environment
	return &e, s.c.Do(ctx, "POST", "environments", nil, p, &e)
}

// Get fetches an environment.
func (s *EnvironmentsService) Get(ctx context.Context, id string) (*Environment, error) {
	var e Environment
	return &e, s.c.Do(ctx, "GET", "environments/"+pathEscape(id), nil, nil, &e)
}

// Update patches an environment.
func (s *EnvironmentsService) Update(ctx context.Context, id string, p EnvironmentParams) (*Environment, error) {
	var e Environment
	return &e, s.c.Do(ctx, "PATCH", "environments/"+pathEscape(id), nil, p, &e)
}

// Delete deletes an environment.
func (s *EnvironmentsService) Delete(ctx context.Context, id string) error {
	return s.c.Do(ctx, "DELETE", "environments/"+pathEscape(id), nil, nil, nil)
}

// ---------- MCP servers ----------

// MCPServersService wraps the team MCP server registry.
type MCPServersService struct{ c *Client }

// MCPServerParams creates or patches a registered server. Nil fields are
// unchanged on Update; an empty string clears URL or Command. Set
// RemoveOAuth to drop the OAuth configuration.
type MCPServerParams struct {
	Name        *string           `json:"name,omitempty"`
	Description *string           `json:"description,omitempty"`
	URL         *string           `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Command     *string           `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	// Connection is "proxy" (the default for a url server) or "direct".
	Connection  *string         `json:"connection,omitempty"`
	OAuth       *MCPOAuthConfig `json:"-"`
	RemoveOAuth bool            `json:"-"`
}

// MarshalJSON encodes OAuth as its configuration fields, or null with RemoveOAuth.
func (p MCPServerParams) MarshalJSON() ([]byte, error) {
	type plain MCPServerParams
	b, err := json.Marshal(plain(p))
	if err != nil {
		return nil, err
	}
	var oauth any
	switch {
	case p.RemoveOAuth:
		oauth = nil
	case p.OAuth != nil:
		oauth = map[string]any{"scopes": p.OAuth.Scopes, "client_id": p.OAuth.ClientID, "client_secret_name": p.OAuth.ClientSecretName,
			"authorization_endpoint": p.OAuth.AuthorizationEndpoint, "token_endpoint": p.OAuth.TokenEndpoint}
	default:
		return b, nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["oauth"] = oauth
	return json.Marshal(m)
}

// List lists the team's registered servers.
func (s *MCPServersService) List(ctx context.Context) ([]RegisteredMCPServer, error) {
	return listAll[RegisteredMCPServer](ctx, s.c, "mcp-servers", "mcp_servers", nil)
}

// Builtin lists the built-in integration servers and whether each is configured.
func (s *MCPServersService) Builtin(ctx context.Context) ([]BuiltinMCPServer, error) {
	var out struct {
		Builtin []BuiltinMCPServer `json:"builtin"`
	}
	return out.Builtin, s.c.Do(ctx, "GET", "mcp-servers", nil, nil, &out)
}

// Create registers a server.
func (s *MCPServersService) Create(ctx context.Context, p MCPServerParams) (*RegisteredMCPServer, error) {
	var m RegisteredMCPServer
	return &m, s.c.Do(ctx, "POST", "mcp-servers", nil, p, &m)
}

// Get fetches a server by id or name.
func (s *MCPServersService) Get(ctx context.Context, ref string) (*RegisteredMCPServer, error) {
	var m RegisteredMCPServer
	return &m, s.c.Do(ctx, "GET", "mcp-servers/"+pathEscape(ref), nil, nil, &m)
}

// Update patches a server.
func (s *MCPServersService) Update(ctx context.Context, ref string, p MCPServerParams) (*RegisteredMCPServer, error) {
	var m RegisteredMCPServer
	return &m, s.c.Do(ctx, "PATCH", "mcp-servers/"+pathEscape(ref), nil, p, &m)
}

// Delete deletes a server and its stored OAuth tokens.
func (s *MCPServersService) Delete(ctx context.Context, ref string) error {
	return s.c.Do(ctx, "DELETE", "mcp-servers/"+pathEscape(ref), nil, nil, nil)
}

// Connect starts the OAuth connection and returns the authorization URL to
// open in a browser where the same user is signed in to the dashboard.
func (s *MCPServersService) Connect(ctx context.Context, ref string) (string, error) {
	var out struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	return out.AuthorizationURL, s.c.Do(ctx, "POST", "mcp-servers/"+pathEscape(ref)+"/oauth/connect", nil, nil, &out)
}

// Disconnect deletes a server's stored OAuth tokens.
func (s *MCPServersService) Disconnect(ctx context.Context, ref string) (*RegisteredMCPServer, error) {
	var m RegisteredMCPServer
	return &m, s.c.Do(ctx, "POST", "mcp-servers/"+pathEscape(ref)+"/oauth/disconnect", nil, nil, &m)
}

// ---------- API keys, users ----------

// APIKeysService wraps API key endpoints.
type APIKeysService struct{ c *Client }

// List lists API keys (own keys; admins see all).
func (s *APIKeysService) List(ctx context.Context) ([]APIKey, error) {
	return listAll[APIKey](ctx, s.c, "api-keys", "api_keys", nil)
}

// Create creates an API key; userID is optional (admins only for other users).
func (s *APIKeysService) Create(ctx context.Context, name, userID string) (*APIKeyCreated, error) {
	var out APIKeyCreated
	return &out, s.c.Do(ctx, "POST", "api-keys", nil, map[string]string{"name": name, "user_id": userID}, &out)
}

// Delete revokes an API key.
func (s *APIKeysService) Delete(ctx context.Context, id string) error {
	return s.c.Do(ctx, "DELETE", "api-keys/"+pathEscape(id), nil, nil, nil)
}

// UsersService wraps identity endpoints.
type UsersService struct{ c *Client }

// Me describes the caller.
func (s *UsersService) Me(ctx context.Context) (*Me, error) {
	var m Me
	return &m, s.c.Do(ctx, "GET", "me", nil, nil, &m)
}

// List lists team members.
func (s *UsersService) List(ctx context.Context) ([]User, error) {
	return listAll[User](ctx, s.c, "users", "users", nil)
}

// Get fetches a user.
func (s *UsersService) Get(ctx context.Context, id string) (*User, error) {
	var u User
	return &u, s.c.Do(ctx, "GET", "users/"+pathEscape(id), nil, nil, &u)
}

// CreateUserResult is the result of a user creation.
type CreateUserResult struct {
	User   User    `json:"user"`
	APIKey *APIKey `json:"api_key,omitempty"`
	Key    string  `json:"key,omitempty"`
}

// Create creates a user (admin only), optionally minting an API key.
func (s *UsersService) Create(ctx context.Context, email, name, role string, createKey bool) (*CreateUserResult, error) {
	var out CreateUserResult
	body := map[string]any{"email": email, "name": name, "role": role, "create_api_key": createKey}
	return &out, s.c.Do(ctx, "POST", "users", nil, body, &out)
}

// Teams lists the caller's teams.
func (s *UsersService) Teams(ctx context.Context) ([]Team, error) {
	return listAll[Team](ctx, s.c, "teams", "teams", nil)
}

// ---------- integrations ----------

// IntegrationsService wraps integration endpoints.
type IntegrationsService struct{ c *Client }

// IntegrationParams creates or patches an integration.
type IntegrationParams struct {
	Provider  string            `json:"provider,omitempty"`
	Name      string            `json:"name,omitempty"`
	Config    map[string]string `json:"config,omitempty"`
	SecretRef string            `json:"secret_ref,omitempty"`
}

// List lists integrations.
func (s *IntegrationsService) List(ctx context.Context) ([]Integration, error) {
	return listAll[Integration](ctx, s.c, "integrations", "integrations", nil)
}

// Create creates an integration (admin only).
func (s *IntegrationsService) Create(ctx context.Context, p IntegrationParams) (*Integration, error) {
	var i Integration
	return &i, s.c.Do(ctx, "POST", "integrations", nil, p, &i)
}

// Get fetches an integration.
func (s *IntegrationsService) Get(ctx context.Context, id string) (*Integration, error) {
	var i Integration
	return &i, s.c.Do(ctx, "GET", "integrations/"+pathEscape(id), nil, nil, &i)
}

// Update patches an integration (admin only).
func (s *IntegrationsService) Update(ctx context.Context, id string, p IntegrationParams) (*Integration, error) {
	var i Integration
	return &i, s.c.Do(ctx, "PATCH", "integrations/"+pathEscape(id), nil, p, &i)
}

// Delete deletes an integration (admin only).
func (s *IntegrationsService) Delete(ctx context.Context, id string) error {
	return s.c.Do(ctx, "DELETE", "integrations/"+pathEscape(id), nil, nil, nil)
}

// ---------- measure ----------

// MeasureService wraps scorers, benchmarks and metrics.
type MeasureService struct{ c *Client }

// Scorers lists a factory's scorers.
func (s *MeasureService) Scorers(ctx context.Context, factoryUID string) ([]Scorer, error) {
	return listAll[Scorer](ctx, s.c, "factories/"+pathEscape(factoryUID)+"/scorers", "scorers", nil)
}

// ScoreListOptions filters scores.
type ScoreListOptions struct {
	Scorer   string // slug or ID
	Agent    string
	Passed   *bool
	Since    *time.Time
	PageSize int
}

// Scores iterates a factory's scores.
func (s *MeasureService) Scores(_ context.Context, factoryUID string, opts *ScoreListOptions) *Iter[Score] {
	q := url.Values{}
	size := 0
	if opts != nil {
		setIf(q, "scorer", opts.Scorer)
		setIf(q, "agent", opts.Agent)
		setIf(q, "since", timeParam(opts.Since))
		if opts.Passed != nil {
			q.Set("passed", strconv.FormatBool(*opts.Passed))
		}
		size = opts.PageSize
	}
	return listIter[Score](s.c, "factories/"+pathEscape(factoryUID)+"/scores", "scores", q, size)
}

// Benchmarks lists a factory's benchmark suites.
func (s *MeasureService) Benchmarks(ctx context.Context, factoryUID string) ([]BenchmarkSuite, error) {
	return listAll[BenchmarkSuite](ctx, s.c, "factories/"+pathEscape(factoryUID)+"/benchmarks", "benchmarks", nil)
}

// BenchmarkConfiguration is one configuration to compare.
type BenchmarkConfiguration struct {
	Name   string            `json:"name"`
	Agents []json.RawMessage `json:"agents,omitempty"`
}

// StartBenchmarkParams launches a suite.
type StartBenchmarkParams struct {
	Configurations []BenchmarkConfiguration `json:"configurations"`
	Scorers        []string                 `json:"scorers,omitempty"`
	Repetitions    int                      `json:"repetitions,omitempty"`
}

// StartBenchmark launches a benchmark suite.
func (s *MeasureService) StartBenchmark(ctx context.Context, factoryUID, suite string, p StartBenchmarkParams) (*BenchmarkRun, error) {
	var b BenchmarkRun
	return &b, s.c.Do(ctx, "POST", "factories/"+pathEscape(factoryUID)+"/benchmarks/"+pathEscape(suite)+"/runs", nil, p, &b)
}

// BenchmarkRuns iterates a factory's benchmark runs (optionally for one suite).
func (s *MeasureService) BenchmarkRuns(_ context.Context, factoryUID, suite string) *Iter[BenchmarkRun] {
	path := "factories/" + pathEscape(factoryUID) + "/benchmark-runs"
	if suite != "" {
		path = "factories/" + pathEscape(factoryUID) + "/benchmarks/" + pathEscape(suite) + "/runs"
	}
	return listIter[BenchmarkRun](s.c, path, "benchmark_runs", nil, 0)
}

// BenchmarkRun fetches a benchmark run.
func (s *MeasureService) BenchmarkRun(ctx context.Context, id string) (*BenchmarkRun, error) {
	var b BenchmarkRun
	return &b, s.c.Do(ctx, "GET", "benchmark-runs/"+pathEscape(id), nil, nil, &b)
}

// Metrics returns dashboard metrics since a time (zero = last 30 days).
func (s *MeasureService) Metrics(ctx context.Context, factoryUID string, since time.Time) (*Metrics, error) {
	q := url.Values{}
	setIf(q, "since", timeParam(&since))
	var m Metrics
	return &m, s.c.Do(ctx, "GET", "factories/"+pathEscape(factoryUID)+"/metrics", q, nil, &m)
}

// ---------- platform ----------

// PlatformService wraps machines, workers, policies and schemas.
type PlatformService struct{ c *Client }

// Machines lists machines declared by factory definitions.
func (s *PlatformService) Machines(ctx context.Context, factoryID string) ([]Machine, error) {
	return listAll[Machine](ctx, s.c, "machines", "machines", scopeQuery(factoryID))
}

// Workers lists workers.
func (s *PlatformService) Workers(ctx context.Context) ([]Worker, error) {
	return listAll[Worker](ctx, s.c, "workers", "workers", nil)
}

// NetworkPolicies returns the built-in network policy catalog.
func (s *PlatformService) NetworkPolicies(ctx context.Context) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, s.c.Do(ctx, "GET", "networking/policies", nil, nil, &raw)
}

// SchemaVersions lists the factory-file schema versions.
func (s *PlatformService) SchemaVersions(ctx context.Context) (current string, versions []string, err error) {
	var out struct {
		Current  string   `json:"current_version"`
		Versions []string `json:"versions"`
	}
	err = s.c.Do(ctx, "GET", "factory-files/schemas", nil, nil, &out)
	return out.Current, out.Versions, err
}

// Schemas returns every schema document of a version, keyed by document name.
func (s *PlatformService) Schemas(ctx context.Context, version string) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	return out, s.c.Do(ctx, "GET", "factory-files/schemas/"+pathEscape(version), nil, nil, &out)
}

// Schema returns one schema document.
func (s *PlatformService) Schema(ctx context.Context, version, document string) (json.RawMessage, error) {
	var raw json.RawMessage
	return raw, s.c.Do(ctx, "GET", "factory-files/schemas/"+pathEscape(version)+"/"+pathEscape(document), nil, nil, &raw)
}

// BootstrapResult is returned by Bootstrap.
type BootstrapResult struct {
	Team   Team   `json:"team"`
	User   User   `json:"user"`
	APIKey APIKey `json:"api_key"`
	Key    string `json:"key"`
}

// Bootstrap creates the first team, admin and API key on an empty server.
func (s *PlatformService) Bootstrap(ctx context.Context, teamName, adminEmail, adminName string) (*BootstrapResult, error) {
	var out BootstrapResult
	body := map[string]string{"team_name": teamName, "admin_email": adminEmail, "admin_name": adminName}
	return &out, s.c.Do(ctx, "POST", "bootstrap", nil, body, &out)
}
