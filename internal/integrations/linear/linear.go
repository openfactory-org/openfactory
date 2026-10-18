// Package linear connects factories to Linear: the webhook receiver
// (Linear-Signature HMAC-SHA256, Issue / Comment / AgentSessionEvent
// payloads), account linking for agent sessions, and a core.Responder that
// posts issue comments (GraphQL commentCreate) or agent-session activities
// (agentActivityCreate).
package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Provider is the trigger and integration provider name.
const Provider = eventattr.ProviderLinear

// WebhookPath is where Linear delivers webhooks.
const WebhookPath = "/integrations/linear/webhook"

// DefaultGraphQLURL is Linear's API endpoint.
const DefaultGraphQLURL = "https://api.linear.app/graphql"

// MaxTimestampSkew bounds webhookTimestamp (replay protection).
const MaxTimestampSkew = time.Minute

// ---------- GraphQL client ----------

// Client is a minimal Linear GraphQL client.
type Client struct{ api *forgeutil.Client }

// NewClient creates a client. tokenType "oauth" sends "Bearer <token>"; otherwise the API key is sent as-is.
func NewClient(endpoint, token, tokenType string, hc *http.Client) *Client {
	if endpoint == "" {
		endpoint = DefaultGraphQLURL
	}
	auth := token
	if strings.EqualFold(tokenType, "oauth") || strings.EqualFold(tokenType, "bearer") {
		auth = "Bearer " + token
	}
	return &Client{api: &forgeutil.Client{
		BaseURL: endpoint, HTTP: hc, UserAgent: "openfactory-linear",
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", auth)
			return nil
		},
	}}
}

// GraphQLError is returned when the response carries errors.
type GraphQLError struct{ Messages []string }

func (e *GraphQLError) Error() string { return "linear graphql: " + strings.Join(e.Messages, "; ") }

// Query runs a GraphQL operation and decodes data into out.
func (c *Client) Query(ctx context.Context, query string, vars map[string]any, out any) error {
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := c.api.Do(ctx, http.MethodPost, "", map[string]any{"query": query, "variables": vars}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		ge := &GraphQLError{}
		for _, e := range resp.Errors {
			ge.Messages = append(ge.Messages, e.Message)
		}
		return ge
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// CreateComment posts a comment on an issue (UUID or identifier such as ENG-42).
func (c *Client) CreateComment(ctx context.Context, issueID, body string) error {
	var out struct {
		CommentCreate struct {
			Success bool `json:"success"`
		} `json:"commentCreate"`
	}
	err := c.Query(ctx, `mutation CommentCreate($input: CommentCreateInput!) { commentCreate(input: $input) { success } }`,
		map[string]any{"input": map[string]any{"issueId": issueID, "body": body}}, &out)
	if err != nil {
		return err
	}
	if !out.CommentCreate.Success {
		return errors.New("linear: commentCreate was not successful")
	}
	return nil
}

// CreateAgentActivity posts an activity into an agent session. kind: thought, action, elicitation, response, error.
func (c *Client) CreateAgentActivity(ctx context.Context, sessionID, kind, body string) error {
	var out struct {
		AgentActivityCreate struct {
			Success bool `json:"success"`
		} `json:"agentActivityCreate"`
	}
	err := c.Query(ctx, `mutation AgentActivityCreate($input: AgentActivityCreateInput!) { agentActivityCreate(input: $input) { success } }`,
		map[string]any{"input": map[string]any{"agentSessionId": sessionID, "content": map[string]any{"type": kind, "body": body}}}, &out)
	if err != nil {
		return err
	}
	if !out.AgentActivityCreate.Success {
		return errors.New("linear: agentActivityCreate was not successful")
	}
	return nil
}

// IssueContext is issue data used to enrich comment and session events.
type IssueContext struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Number     int    `json:"number"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Team       *named `json:"team"`
	Project    *named `json:"project"`
	State      *named `json:"state"`
	Labels     struct {
		Nodes []named `json:"nodes"`
	} `json:"labels"`
}

type named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// Issue loads issue context.
func (c *Client) Issue(ctx context.Context, id string) (*IssueContext, error) {
	var out struct {
		Issue IssueContext `json:"issue"`
	}
	err := c.Query(ctx, `query Issue($id: String!) { issue(id: $id) { id identifier number title url
		team { id name key } project { id name } state { id name } labels { nodes { id name } } } }`,
		map[string]any{"id": id}, &out)
	if err != nil {
		return nil, err
	}
	return &out.Issue, nil
}

// ---------- Webhook ----------

// Options configures the webhook receiver and responder.
//
// Integration records (provider "linear"):
//   - config organization_id: Linear workspace (organization) ID, necessary.
//     One record serves one workspace. The record of a factory (the
//     record's factory_id) wins over the team record.
//   - config app_user_id: the app's user ID (its own activity is ignored)
//   - config token_type: "oauth" for OAuth access tokens (default: API key)
//   - config webhook_secret_ref: secret name holding the webhook signing secret
//   - SecretRef: access token / API key
type Options struct {
	Store      core.Store
	Secrets    core.SecretStore
	Bus        core.EventBus
	GraphQLURL string
	HTTPClient *http.Client
	Linker     *integrations.Linker
	Logger     *slog.Logger
	Now        func() time.Time
}

// Handler receives Linear webhooks and implements core.Responder.
type Handler struct {
	opts  Options
	log   *slog.Logger
	dedup *forgeutil.Dedupe
	wg    sync.WaitGroup
}

var _ core.Responder = (*Handler)(nil)

// NewHandler creates the handler.
func NewHandler(opts Options) *Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "linear"), dedup: forgeutil.NewDedupe(time.Hour)}
}

// Register mounts POST /integrations/linear/webhook.
func (h *Handler) Register(mux *http.ServeMux) { mux.HandleFunc("POST "+WebhookPath, h.serveWebhook) }

// Drain waits for in-flight background processing.
func (h *Handler) Drain() { h.wg.Wait() }

type actorRef struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

type issueData struct {
	ID          string   `json:"id"`
	Identifier  string   `json:"identifier"`
	Number      int      `json:"number"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	TeamID      string   `json:"teamId"`
	Team        *named   `json:"team"`
	ProjectID   string   `json:"projectId"`
	Project     *named   `json:"project"`
	StateID     string   `json:"stateId"`
	State       *named   `json:"state"`
	AssigneeID  string   `json:"assigneeId"`
	CreatorID   string   `json:"creatorId"`
	LabelIDs    []string `json:"labelIds"`
	Labels      []named  `json:"labels"`
}

type payload struct {
	Action           string                     `json:"action"`
	Type             string                     `json:"type"`
	Data             json.RawMessage            `json:"data"`
	UpdatedFrom      map[string]json.RawMessage `json:"updatedFrom"`
	URL              string                     `json:"url"`
	OrganizationID   string                     `json:"organizationId"`
	WebhookTimestamp int64                      `json:"webhookTimestamp"`
	WebhookID        string                     `json:"webhookId"`
	Actor            *actorRef                  `json:"actor"`
	AppUserID        string                     `json:"appUserId"`
	AgentSession     *struct {
		ID        string     `json:"id"`
		IssueID   string     `json:"issueId"`
		Issue     *issueData `json:"issue"`
		CreatorID string     `json:"creatorId"`
		Creator   *actorRef  `json:"creator"`
		Comment   *struct {
			ID   string `json:"id"`
			Body string `json:"body"`
		} `json:"comment"`
	} `json:"agentSession"`
	AgentActivity *struct {
		Content struct {
			Type string `json:"type"`
			Body string `json:"body"`
		} `json:"content"`
		UserID string `json:"userId"`
	} `json:"agentActivity"`
	PromptContext string `json:"promptContext"`
}

func (h *Handler) serveWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	ctx := r.Context()
	integ, ierr := h.findIntegration(ctx, p.OrganizationID)
	if !h.verify(ctx, integ, body, r.Header.Get("Linear-Signature")) {
		forgeutil.WriteError(w, http.StatusUnauthorized, "invalid_signature", "Linear-Signature does not match")
		return
	}
	if p.WebhookTimestamp != 0 {
		skew := h.opts.Now().Sub(time.UnixMilli(p.WebhookTimestamp))
		if skew > MaxTimestampSkew || skew < -MaxTimestampSkew {
			forgeutil.WriteError(w, http.StatusUnauthorized, "stale_webhook", "webhookTimestamp is outside the allowed window")
			return
		}
	}
	if ierr != nil {
		h.log.Info("delivery ignored", "org", p.OrganizationID, "err", ierr)
		forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "ignored", "reason": "no matching integration"})
		return
	}
	delivery := r.Header.Get("Linear-Delivery")
	if h.dedup.Seen(delivery) {
		forgeutil.WriteJSON(w, http.StatusOK, map[string]any{"status": "duplicate"})
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		evs, err := h.Normalize(bctx, *integ, delivery, body)
		if err != nil {
			h.log.Warn("normalize linear webhook", "err", err)
			return
		}
		forgeutil.Publish(bctx, h.opts.Bus, evs)
	}()
	forgeutil.WriteJSON(w, http.StatusAccepted, map[string]any{"status": "accepted"})
}

func (h *Handler) verify(ctx context.Context, integ *core.Integration, body []byte, sig string) bool {
	if sig == "" {
		return false
	}
	if integ != nil && integ.Config["webhook_secret_ref"] != "" {
		sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, *integ, "webhook_secret_ref")
		if err != nil {
			h.log.Warn("load webhook secret", "integration", integ.ID, "err", err)
			return false
		}
		return forgeutil.VerifyHMACSHA256Hex(sec, body, sig)
	}
	return false
}

// findIntegration selects the record of a webhook delivery with the
// selection rule: the records whose organization_id
// is the organization of the delivery. In the team, the team record wins
// over a factory record.
func (h *Handler) findIntegration(ctx context.Context, orgID string) (*core.Integration, error) {
	return integrations.Intake(ctx, h.opts.Store, Provider, orgID, nil)
}

func (h *Handler) client(ctx context.Context, in core.Integration) (*Client, error) {
	tok, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, in, "")
	if err != nil {
		return nil, err
	}
	return NewClient(h.opts.GraphQLURL, string(tok), in.Config["token_type"], h.opts.HTTPClient), nil
}

var (
	uuidRe        = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`
	profileURLRe  = regexp.MustCompile(`https?://linear\.app/[^/\s)]+/profiles/([A-Za-z0-9._-]+)`)
	mentionUUIDRe = regexp.MustCompile(`(?i)(?:mention[^>\]]*?|@\[[^\]]*\]\()(` + uuidRe + `)`)
)

// ExtractMentions returns user UUIDs from mention markup and profile slugs from profile URLs.
func ExtractMentions(text string) []string {
	var out []string
	for _, m := range mentionUUIDRe.FindAllStringSubmatch(text, -1) {
		out = append(out, strings.ToLower(m[1]))
	}
	for _, m := range profileURLRe.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return forgeutil.StringSet(out...)
}

func labelNames(ls []named) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func idsOf(raw json.RawMessage) ([]string, bool) {
	if raw == nil {
		return nil, false
	}
	var ids []string
	_ = json.Unmarshal(raw, &ids)
	return ids, true
}

func (h *Handler) issueAttrs(is *issueData, labels []string) map[string]any {
	a := map[string]any{
		eventattr.IssueID:     is.ID,
		eventattr.IssueNumber: is.Number,
		eventattr.Labels:      labels,
		eventattr.Author:      is.CreatorID,
	}
	setNamed := func(idKey, nameKey, id string, n *named) {
		if n != nil && n.ID != "" {
			id = n.ID
		}
		if id != "" {
			a[idKey] = id
		}
		if n != nil && n.Name != "" {
			a[nameKey] = n.Name
		}
	}
	setNamed(eventattr.TeamID, eventattr.TeamName, is.TeamID, is.Team)
	setNamed(eventattr.ProjectID, eventattr.ProjectName, is.ProjectID, is.Project)
	setNamed(eventattr.StateID, eventattr.StateName, is.StateID, is.State)
	if is.AssigneeID != "" {
		a[eventattr.Assignees] = []string{is.AssigneeID}
	} else {
		a[eventattr.Assignees] = []string{}
	}
	return a
}

func (h *Handler) enrich(ctx context.Context, c *Client, issueID string, is *issueData) []string {
	if c == nil || issueID == "" {
		return labelNames(is.Labels)
	}
	ic, err := c.Issue(ctx, issueID)
	if err != nil {
		h.log.Debug("load linear issue", "issue", issueID, "err", err)
		return labelNames(is.Labels)
	}
	is.ID, is.Identifier, is.Number, is.URL = ic.ID, ic.Identifier, ic.Number, ic.URL
	if is.Title == "" {
		is.Title = ic.Title
	}
	is.Team, is.Project, is.State = ic.Team, ic.Project, ic.State
	return labelNames(ic.Labels.Nodes)
}

// Normalize converts a verified webhook payload into events (enriching via GraphQL when a token is configured).
func (h *Handler) Normalize(ctx context.Context, in core.Integration, deliveryID string, body []byte) ([]*core.Event, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("linear: decode payload: %w", err)
	}
	appUser := in.Config["app_user_id"]
	c, err := h.client(ctx, in)
	if err != nil {
		c = nil
	}
	factoryID := in.FactoryID
	workspace := core.IntegrationWorkspace(in)
	var out []*core.Event
	ownApps := map[string]bool{}
	if appUser != "" {
		ownApps[appUser] = true
	}
	if h.opts.Store != nil {
		if all, err := forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, nil); err == nil {
			for _, o := range all {
				if u := o.Config["app_user_id"]; u != "" {
					ownApps[u] = true
				}
			}
		}
	}
	emit := func(typ string, actor core.Actor, src core.SourceRef, text string, attrs map[string]any) {
		self := actor.ExternalID != "" && ownApps[actor.ExternalID]
		if self {
			actor.IsBot = true
		}
		attrs[eventattr.ActorIsSelf] = self
		attrs[eventattr.ActorIsBot] = actor.IsBot
		if text != "" {
			attrs[eventattr.Text] = text
		}
		did := deliveryID
		if did != "" {
			did += ":" + typ
		}
		out = append(out, &core.Event{
			ID: core.NewID("evt"), TeamID: in.TeamID, FactoryID: factoryID, Provider: Provider, Type: typ,
			DeliveryID: did, Actor: actor, Source: src, Attributes: attrs, Payload: json.RawMessage(body),
			Text: text, ReceivedAt: h.opts.Now().UTC(),
		})
	}
	actorOf := func(id, name, typ string) core.Actor {
		a := core.Actor{Provider: Provider, ExternalID: id, Handle: name, IsBot: typ != "" && typ != "user"}
		forgeutil.LinkActor(ctx, h.opts.Store, &a)
		return a
	}
	issueSource := func(is *issueData) core.SourceRef {
		return core.SourceRef{Provider: Provider, Kind: "issue", URL: is.URL, Channel: is.ID, ExternalID: is.Identifier, Title: is.Title, Workspace: workspace}
	}
	pActor := core.Actor{}
	if p.Actor != nil {
		pActor = actorOf(p.Actor.ID, p.Actor.Name, p.Actor.Type)
	}
	// Activity by the app itself never starts work.
	if appUser != "" && pActor.ExternalID == appUser {
		return nil, nil
	}

	switch p.Type {
	case "Issue":
		var is issueData
		if err := json.Unmarshal(p.Data, &is); err != nil {
			return nil, fmt.Errorf("linear: decode issue: %w", err)
		}
		if is.URL == "" {
			is.URL = p.URL
		}
		labels := labelNames(is.Labels)
		base := h.issueAttrs(&is, labels)
		base[eventattr.Mentioned] = ExtractMentions(is.Description)
		src := issueSource(&is)
		text := strings.TrimSpace(is.Title + "\n\n" + is.Description)
		if pActor.ExternalID == "" {
			pActor = actorOf(is.CreatorID, "", "user")
		}
		cp := func() map[string]any {
			m := make(map[string]any, len(base))
			for k, v := range base {
				m[k] = v
			}
			return m
		}
		switch p.Action {
		case "create":
			emit("issue_created", pActor, src, text, cp())
			if len(labels) > 0 {
				a := cp()
				a[eventattr.AddedLabels] = labels
				emit("issue_labeled", pActor, src, text, a)
			}
			if is.AssigneeID != "" {
				emit("issue_assigned", pActor, src, text, cp())
			}
		case "update":
			if prev, ok := idsOf(p.UpdatedFrom["labelIds"]); ok {
				addedIDs := forgeutil.Diff(is.LabelIDs, prev)
				var added []string
				for _, l := range is.Labels {
					for _, id := range addedIDs {
						if l.ID == id {
							added = append(added, l.Name)
						}
					}
				}
				if len(added) > 0 {
					a := cp()
					a[eventattr.AddedLabels] = added
					emit("issue_labeled", pActor, src, text, a)
				}
			}
			if _, ok := p.UpdatedFrom["assigneeId"]; ok && is.AssigneeID != "" {
				emit("issue_assigned", pActor, src, text, cp())
			}
			if _, ok := p.UpdatedFrom["stateId"]; ok {
				emit("issue_state_changed", pActor, src, text, cp())
			}
		}
	case "Comment":
		if p.Action != "create" {
			return nil, nil
		}
		var cm struct {
			ID      string     `json:"id"`
			Body    string     `json:"body"`
			IssueID string     `json:"issueId"`
			Issue   *issueData `json:"issue"`
			UserID  string     `json:"userId"`
			User    *actorRef  `json:"user"`
		}
		if err := json.Unmarshal(p.Data, &cm); err != nil {
			return nil, fmt.Errorf("linear: decode comment: %w", err)
		}
		if appUser != "" && cm.UserID == appUser {
			return nil, nil
		}
		is := issueData{ID: cm.IssueID}
		if cm.Issue != nil {
			is = *cm.Issue
			if is.ID == "" {
				is.ID = cm.IssueID
			}
		}
		labels := h.enrich(ctx, c, is.ID, &is)
		a := h.issueAttrs(&is, labels)
		delete(a, eventattr.Assignees)
		a[eventattr.Author] = cm.UserID
		a[eventattr.Mentioned] = ExtractMentions(cm.Body)
		a["comment_id"] = cm.ID
		actor := pActor
		if actor.ExternalID == "" {
			name := ""
			if cm.User != nil {
				name = cm.User.Name
			}
			actor = actorOf(cm.UserID, name, "user")
		}
		emit("comment_created", actor, issueSource(&is), cm.Body, a)
	case "AgentSessionEvent":
		s := p.AgentSession
		if s == nil {
			return nil, nil
		}
		is := issueData{ID: s.IssueID}
		if s.Issue != nil {
			is = *s.Issue
		}
		labels := h.enrich(ctx, c, is.ID, &is)
		a := h.issueAttrs(&is, labels)
		delete(a, eventattr.Assignees)
		a[eventattr.Author] = s.CreatorID
		a[eventattr.SessionID] = s.ID
		creatorName := ""
		if s.Creator != nil {
			creatorName = s.Creator.Name
		}
		actor := actorOf(s.CreatorID, creatorName, "user")
		src := core.SourceRef{Provider: Provider, Kind: "agent_session", URL: is.URL, Channel: is.ID, ThreadID: s.ID, ExternalID: is.Identifier, Title: is.Title, Workspace: workspace}
		var text string
		var typ string
		switch p.Action {
		case "created":
			typ = "agent_session_created"
			if s.Comment != nil {
				text = s.Comment.Body
			}
			if text == "" {
				text = p.PromptContext
			}
		case "prompted":
			typ = "agent_session_prompted" // follow-up in an existing session
			if p.AgentActivity != nil {
				text = p.AgentActivity.Content.Body
				if p.AgentActivity.UserID != "" {
					actor = actorOf(p.AgentActivity.UserID, "", "user")
				}
			}
		default:
			return nil, nil
		}
		// Agent sessions require a linked account of an active team member.
		member, err := integrations.Member(ctx, h.opts.Store, in.TeamID, Provider, actor.ExternalID)
		if err != nil {
			h.log.Warn("membership lookup", "err", err)
		}
		if member == nil {
			if c != nil {
				// The agent session is visible to the workspace, so the
				// prompt has no link URL.
				msg := "Link your Linear account to an active member of this factory's Open Factory team, then retry the request. " + h.opts.Linker.DashboardHint(Provider)
				if err := c.CreateAgentActivity(ctx, s.ID, "elicitation", msg); err != nil {
					h.log.Warn("post authentication prompt", "err", err)
				}
			}
			return nil, nil
		}
		actor.UserID = member.ID
		a[eventattr.Linked] = true
		emit(typ, actor, src, text, a)
	}
	return out, nil
}

// Provider implements core.Responder.
func (h *Handler) Provider() string { return Provider }

// Reply implements core.Responder: agent-session sources receive agent activities; issues receive comments.
func (h *Handler) Reply(ctx context.Context, teamID string, msg core.OutboundMessage) error {
	c, err := h.teamClient(core.WithFactory(ctx, msg.FactoryID), teamID, msg.Source)
	if err != nil {
		return err
	}
	body := forgeutil.FormatOutbound(msg)
	if msg.Source.Kind == "agent_session" && msg.Source.ThreadID != "" {
		kind := "response"
		switch msg.Kind {
		case "question", "approval_request":
			kind = "elicitation"
		case "error":
			kind = "error"
		case "update":
			kind = "thought"
		}
		return c.CreateAgentActivity(ctx, msg.Source.ThreadID, kind, body)
	}
	issue := msg.Source.Channel
	if issue == "" {
		issue = msg.Source.ExternalID
	}
	if issue == "" {
		return errors.New("linear: outbound message has no issue")
	}
	return c.CreateComment(ctx, issue, body)
}
