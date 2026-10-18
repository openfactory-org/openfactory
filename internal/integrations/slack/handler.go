package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Provider is the trigger provider and integration provider name.
const Provider = eventattr.ProviderSlack

// Endpoints.
const (
	EventsPath       = "/integrations/slack/events"
	InteractionsPath = "/integrations/slack/interactions"
)

// DefaultTimestampTolerance bounds the age of signed requests (replay protection).
const DefaultTimestampTolerance = 5 * time.Minute

// Options configures the Slack handler.
//
// Integration records (provider "slack"), one per workspace and factory:
//   - config team_id: Slack workspace ID (T…), necessary. One record serves
//     one workspace. The record of a factory (the record's factory_id) wins
//     over the team record; events of a factory record are factory-scoped.
//   - config api_app_id: the Slack app ID (A…)
//   - config bot_user_id: the app's bot user ID (U…); its own posts are ignored
//   - config signing_secret_ref: secret name holding the app's signing secret
//   - SecretRef: secret name holding the bot token (xoxb-…)
type Options struct {
	Store   core.Store
	Secrets core.SecretStore
	Bus     core.EventBus
	// APIBaseURL overrides https://slack.com/api (tests).
	APIBaseURL string
	HTTPClient *http.Client
	// Linker builds account-link prompts for unlinked users.
	Linker *integrations.Linker
	// OnAction resolves an inbox item when a user presses an approval / option button.
	// actor is the Open Factory user ID of the linked Slack user.
	OnAction           func(ctx context.Context, inboxID, option, actor string) error
	TimestampTolerance time.Duration
	Logger             *slog.Logger
	Now                func() time.Time
}

// Handler serves the Slack Events API and interactivity endpoints.
type Handler struct {
	opts  Options
	log   *slog.Logger
	dedup *forgeutil.Dedupe
	wg    sync.WaitGroup

	mu       sync.Mutex
	users    map[string]cachedUser
	channels map[string]cachedName
}

type cachedUser struct {
	u       *UserInfo
	expires time.Time
}

type cachedName struct {
	name    string
	expires time.Time
}

// NewHandler creates the handler.
func NewHandler(opts Options) *Handler {
	if opts.TimestampTolerance <= 0 {
		opts.TimestampTolerance = DefaultTimestampTolerance
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Handler{
		opts: opts, log: forgeutil.Logger(opts.Logger).With("component", "slack"),
		dedup: forgeutil.NewDedupe(time.Hour),
		users: map[string]cachedUser{}, channels: map[string]cachedName{},
	}
}

// Register mounts the events and interactions endpoints.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST "+EventsPath, h.serveEvents)
	mux.HandleFunc("POST "+InteractionsPath, h.serveInteractions)
}

// Drain waits for in-flight background processing.
func (h *Handler) Drain() { h.wg.Wait() }

// Sign computes the v0 signature for a request (exported for tests and tooling).
func Sign(secret []byte, timestamp string, body []byte) string {
	return "v0=" + forgeutil.HMACSHA256Hex(secret, []byte("v0:"+timestamp+":"+string(body)))
}

func (h *Handler) checkSignature(secret []byte, r *http.Request, body []byte) bool {
	ts := r.Header.Get("X-Slack-Request-Timestamp")
	sig := r.Header.Get("X-Slack-Signature")
	if len(secret) == 0 || ts == "" || !strings.HasPrefix(sig, "v0=") {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return false
	}
	age := h.opts.Now().Sub(time.Unix(n, 0))
	if age > h.opts.TimestampTolerance || age < -h.opts.TimestampTolerance {
		return false
	}
	return forgeutil.ConstantTimeEqual(Sign(secret, ts, body), sig)
}

// verify checks the request against the matching integration's secret,
// or (when appID is unknown, e.g. url_verification) every Slack integration's secret.
func (h *Handler) verify(ctx context.Context, r *http.Request, body []byte, integ *core.Integration) bool {
	var cands []core.Integration
	if integ != nil {
		cands = []core.Integration{*integ}
	} else if h.opts.Store != nil {
		cands, _ = forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, nil)
	}
	for _, in := range cands {
		if in.Config["signing_secret_ref"] == "" {
			continue
		}
		sec, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, in, "signing_secret_ref")
		if err != nil {
			h.log.Warn("load signing secret", "integration", in.ID, "err", err)
			continue
		}
		if h.checkSignature(sec, r, body) {
			return true
		}
	}
	return false
}

// findIntegration selects the record of an event or an interaction with the
// selection rule: the records whose team_id is the
// workspace of the event. When records name the app of the event
// (api_app_id), only those records can have received it. In the team, the
// team record wins over a factory record.
func (h *Handler) findIntegration(ctx context.Context, appID, teamID string) (*core.Integration, error) {
	if h.opts.Store == nil {
		return nil, errors.New("no store configured")
	}
	exact := func(in core.Integration) bool {
		return appID != "" && strings.EqualFold(in.Config["api_app_id"], appID)
	}
	named, err := forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, func(in core.Integration) bool {
		return exact(in) && strings.EqualFold(core.IntegrationWorkspace(in), teamID)
	})
	if err != nil {
		return nil, err
	}
	match := func(in core.Integration) bool { return in.Config["api_app_id"] == "" || exact(in) }
	if len(named) > 0 {
		match = exact
	}
	return integrations.Intake(ctx, h.opts.Store, Provider, teamID, match)
}

func (h *Handler) client(ctx context.Context, in core.Integration) (*Client, error) {
	tok, err := forgeutil.IntegrationSecret(ctx, h.opts.Secrets, in, "")
	if err != nil {
		return nil, err
	}
	return NewClient(h.opts.APIBaseURL, string(tok), h.opts.HTTPClient), nil
}

type eventEnvelope struct {
	Type      string          `json:"type"`
	Challenge string          `json:"challenge"`
	TeamID    string          `json:"team_id"`
	APIAppID  string          `json:"api_app_id"`
	EventID   string          `json:"event_id"`
	EventTime int64           `json:"event_time"`
	Event     json.RawMessage `json:"event"`
}

func (h *Handler) serveEvents(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	var env eventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "payload is not valid JSON")
		return
	}
	ctx := r.Context()
	var integ *core.Integration
	var ierr error
	if env.Type != "url_verification" {
		integ, ierr = h.findIntegration(ctx, env.APIAppID, env.TeamID)
	}
	if !h.verify(ctx, r, body, integ) {
		forgeutil.WriteError(w, http.StatusUnauthorized, "invalid_signature", "Slack request signature verification failed")
		return
	}
	switch env.Type {
	case "url_verification":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(env.Challenge))
		return
	case "event_callback":
	default:
		w.WriteHeader(http.StatusOK)
		return
	}
	if ierr != nil {
		h.log.Info("event ignored", "app", env.APIAppID, "team", env.TeamID, "err", ierr)
		w.WriteHeader(http.StatusOK)
		return
	}
	if h.dedup.Seen(env.EventID) {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Slack expects an answer within 3 seconds; do the work in the background.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := h.handleEvent(bctx, *integ, env); err != nil {
			h.log.Warn("handle slack event", "event_id", env.EventID, "err", err)
		}
	}()
	w.WriteHeader(http.StatusOK)
}

type slackEvent struct {
	Type        string            `json:"type"`
	Subtype     string            `json:"subtype"`
	User        string            `json:"user"`
	BotID       string            `json:"bot_id"`
	AppID       string            `json:"app_id"`
	Text        string            `json:"text"`
	TS          string            `json:"ts"`
	ThreadTS    string            `json:"thread_ts"`
	Channel     string            `json:"channel"`
	ChannelType string            `json:"channel_type"`
	EventTS     string            `json:"event_ts"`
	Blocks      []json.RawMessage `json:"blocks"`
	Attachments []json.RawMessage `json:"attachments"`
	BotProfile  *struct {
		AppID string `json:"app_id"`
	} `json:"bot_profile"`
	// reaction_added
	Reaction string `json:"reaction"`
	ItemUser string `json:"item_user"`
	Item     *struct {
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
	// member_joined_channel
	Inviter string `json:"inviter"`
}

func (h *Handler) userInfo(ctx context.Context, c *Client, id string) *UserInfo {
	if id == "" || c == nil {
		return nil
	}
	h.mu.Lock()
	cu, ok := h.users[id]
	h.mu.Unlock()
	if ok && h.opts.Now().Before(cu.expires) {
		return cu.u
	}
	u, err := c.UserInfo(ctx, id)
	if err != nil {
		h.log.Debug("users.info", "user", id, "err", err)
		return nil
	}
	h.mu.Lock()
	h.users[id] = cachedUser{u: u, expires: h.opts.Now().Add(10 * time.Minute)}
	h.mu.Unlock()
	return u
}

func (h *Handler) channelName(ctx context.Context, c *Client, id string) string {
	if id == "" || c == nil || strings.HasPrefix(id, "D") {
		return ""
	}
	h.mu.Lock()
	cn, ok := h.channels[id]
	h.mu.Unlock()
	if ok && h.opts.Now().Before(cn.expires) {
		return cn.name
	}
	name, err := c.ChannelName(ctx, id)
	if err != nil {
		h.log.Debug("conversations.info", "channel", id, "err", err)
		return ""
	}
	h.mu.Lock()
	h.channels[id] = cachedName{name: name, expires: h.opts.Now().Add(10 * time.Minute)}
	h.mu.Unlock()
	return name
}

// isOwnApp reports whether the event was posted by any Open Factory Slack app
// registered on this server (for example another factory's app in the same
// workspace). Such posts are marked actor_is_self so they never trigger work.
func (h *Handler) isOwnApp(ctx context.Context, ev slackEvent) bool {
	if h.opts.Store == nil || (ev.BotID == "" && ev.AppID == "" && ev.BotProfile == nil) {
		return false
	}
	all, err := forgeutil.FindIntegrations(ctx, h.opts.Store, Provider, nil)
	if err != nil {
		return false
	}
	for _, in := range all {
		if h.isSelf(in, ev) {
			return true
		}
	}
	return false
}

func (h *Handler) isSelf(in core.Integration, ev slackEvent) bool {
	appID := in.Config["api_app_id"]
	if bu := in.Config["bot_user_id"]; bu != "" && ev.User == bu {
		return true
	}
	if appID == "" {
		return false
	}
	return ev.AppID == appID || (ev.BotProfile != nil && ev.BotProfile.AppID == appID)
}

// MessageURL builds a permalink-style URL for a message.
func MessageURL(channel, ts string) string {
	if channel == "" || ts == "" {
		return ""
	}
	return "https://slack.com/archives/" + channel + "/p" + strings.ReplaceAll(ts, ".", "")
}

func (h *Handler) handleEvent(ctx context.Context, in core.Integration, env eventEnvelope) error {
	var ev slackEvent
	if err := json.Unmarshal(env.Event, &ev); err != nil {
		return fmt.Errorf("decode event: %w", err)
	}
	if h.isSelf(in, ev) {
		return nil // posts by the receiving app are ignored
	}
	c, err := h.client(ctx, in)
	if err != nil {
		h.log.Warn("slack bot token unavailable; continuing without Web API enrichment", "integration", in.ID, "err", err)
		c = nil
	}
	factoryID := in.FactoryID

	var typ string
	requireLink := false
	channel := ev.Channel
	threadTS := ev.ThreadTS
	if threadTS == "" {
		threadTS = ev.TS
	}
	text := strings.TrimSpace(ev.Text + "\n" + TextOfBlocks(ev.Blocks, ev.Attachments))
	actorID := ev.User
	attrs := map[string]any{}
	switch ev.Type {
	case "app_mention":
		if ev.Subtype != "" && ev.Subtype != "thread_broadcast" && ev.Subtype != "file_share" {
			return nil
		}
		typ, requireLink = "app_mention", true
	case "message":
		switch ev.Subtype {
		case "", "file_share", "thread_broadcast":
		default:
			return nil // edits, deletions, joins and other system messages
		}
		switch ev.ChannelType {
		case "im":
			typ, requireLink = "message_im", true
		case "mpim":
			typ, requireLink = "message_mpim", true
		default:
			typ = "message_posted"
		}
	case "reaction_added":
		if ev.Item == nil || ev.Item.Type != "message" {
			return nil
		}
		typ = "reaction_added"
		channel = ev.Item.Channel
		threadTS = ev.Item.TS
		text = ""
		attrs[eventattr.Emoji] = ev.Reaction
		attrs[eventattr.ItemUserID] = ev.ItemUser
		if iu := h.userInfo(ctx, c, ev.ItemUser); iu != nil {
			attrs[eventattr.ItemUserName] = iu.Name
			attrs[eventattr.ItemUserEmail] = iu.Profile.Email
		}
		if c != nil {
			if m, err := c.GetMessage(ctx, ev.Item.Channel, ev.Item.TS); err == nil {
				text = strings.TrimSpace(m.Text + "\n" + TextOfBlocks(m.Blocks, m.Attachments))
				if m.ThreadTS != "" {
					threadTS = m.ThreadTS
				}
			} else {
				h.log.Debug("fetch reacted message", "err", err)
			}
		}
	case "member_joined_channel":
		typ = "member_joined_channel"
		threadTS = ""
		text = ""
	default:
		return nil
	}

	user := h.userInfo(ctx, c, actorID)
	isBot := ev.BotID != "" || ev.AppID != "" || (user != nil && user.IsBot)
	actor := core.Actor{Provider: Provider, ExternalID: actorID, IsBot: isBot}
	if user != nil {
		actor.Handle = user.Name
	}

	// Who can start work: mentions and DMs need a Slack account linked to an active team member.
	member, err := integrations.Member(ctx, h.opts.Store, in.TeamID, Provider, actorID)
	if err != nil {
		h.log.Warn("membership lookup", "user", actorID, "err", err)
	}
	if member == nil && user != nil && user.Profile.Email != "" && !isBot && linkByEmail(in) {
		ws := in.Config["team_id"]
		c := integrations.EmailCandidate{
			Provider: Provider, ExternalID: actorID, Handle: user.Name, Email: user.Profile.Email,
			Guest:   user.IsRestricted || user.IsUltraRestricted,
			Foreign: user.IsStranger || (ws != "" && user.TeamID != ws) || (ws == "" && user.TeamID == ""),
			Bot:     user.IsBot, Deleted: user.Deleted,
		}
		if member, err = h.opts.Linker.LinkByEmail(ctx, h.opts.Store, in.TeamID, c); err != nil {
			h.log.Warn("link by email", "user", actorID, "err", err)
		}
	}
	if member != nil {
		actor.UserID = member.ID
	}
	if requireLink && member == nil {
		h.promptLink(ctx, c, in, channel, ev, actor)
		return nil
	}

	ws := core.IntegrationWorkspace(in)
	source := core.SourceRef{Provider: Provider, Kind: "thread", Channel: channel, ThreadID: threadTS, URL: MessageURL(channel, threadTS), Workspace: ws}
	if typ == "member_joined_channel" {
		source = core.SourceRef{Provider: Provider, Kind: "channel", Channel: channel, ExternalID: channel + ":" + actorID + ":" + ev.EventTS, Workspace: ws}
	}
	attrs[eventattr.ChannelID] = channel
	attrs[eventattr.ChannelType] = ev.ChannelType
	attrs[eventattr.UserID] = actorID
	attrs[eventattr.ActorIsBot] = isBot
	attrs[eventattr.ActorIsSelf] = h.isOwnApp(ctx, ev)
	attrs[eventattr.Linked] = member != nil
	if user != nil {
		attrs[eventattr.UserName] = user.Name
		attrs[eventattr.UserEmail] = user.Profile.Email
	}
	if name := h.channelName(ctx, c, channel); name != "" {
		attrs[eventattr.ChannelName] = name
	}
	if ev.Type == "message" && ev.ThreadTS != "" && ev.ThreadTS != ev.TS {
		attrs[eventattr.ThreadReply] = true
		attrs[eventattr.AutoFollowUp] = integrations.AutoRespondToThreadReplies(ctx, h.opts.Store, factoryID, "slack")
	}
	if text != "" {
		attrs[eventattr.Text] = text
	}
	out := &core.Event{
		ID: core.NewID("evt"), TeamID: in.TeamID, FactoryID: factoryID, Provider: Provider, Type: typ,
		DeliveryID: env.EventID, Actor: actor, Source: source, Attributes: attrs, Payload: env.Event,
		Text: text, ReceivedAt: h.opts.Now().UTC(),
	}
	if h.opts.Bus != nil {
		h.opts.Bus.Publish(ctx, core.TopicInboundEvent, out)
	}
	// Acknowledge explicit requests with 👀.
	if requireLink && c != nil {
		if err := c.AddReaction(ctx, channel, ev.TS, "eyes"); err != nil {
			h.log.Debug("acknowledge reaction", "err", err)
		}
	}
	return nil
}

// ConfigLinkByEmail is the config key of the opt-in setting that links a
// Slack account to the team member with the same email address.
const ConfigLinkByEmail = "link_by_email"

// linkByEmail reports whether the integration links accounts by email
// address. It is off by default.
func linkByEmail(in core.Integration) bool {
	switch strings.ToLower(strings.TrimSpace(in.Config[ConfigLinkByEmail])) {
	case "true", "1", "yes":
		return true
	}
	return false
}

func (h *Handler) promptLink(ctx context.Context, c *Client, in core.Integration, channel string, ev slackEvent, actor core.Actor) {
	if c == nil {
		return
	}
	msg := "To start work from Slack, link your Slack account to an active member of this factory's Open Factory team, then send your request again."
	// An ephemeral message is private to the user, so it can carry the URL.
	if u := h.opts.Linker.PrivateURL(in.TeamID, Provider, actor.ExternalID, actor.Handle); u != "" {
		msg = "To start work from Slack, <" + u + "|link your Slack account> to your Open Factory account, then send your request again."
	}
	m := Message{Channel: channel, User: actor.ExternalID, Text: msg}
	if ev.ThreadTS != "" {
		m.ThreadTS = ev.ThreadTS
	}
	if err := c.PostEphemeral(ctx, m); err != nil {
		h.log.Warn("post link prompt", "err", err)
	}
}

// ---------- Interactivity ----------

type actionValue struct {
	WorkItemID string `json:"w,omitempty"`
	InboxID    string `json:"i,omitempty"`
	Option     string `json:"o"`
}

type interaction struct {
	Type     string `json:"type"`
	APIAppID string `json:"api_app_id"`
	Team     struct {
		ID string `json:"id"`
	} `json:"team"`
	User struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Message struct {
		TS       string           `json:"ts"`
		ThreadTS string           `json:"thread_ts"`
		Text     string           `json:"text"`
		Blocks   []map[string]any `json:"blocks"`
	} `json:"message"`
	ResponseURL string `json:"response_url"`
	Actions     []struct {
		ActionID string `json:"action_id"`
		Value    string `json:"value"`
	} `json:"actions"`
}

func (h *Handler) serveInteractions(w http.ResponseWriter, r *http.Request) {
	body, err := forgeutil.ReadBody(r)
	if err != nil {
		forgeutil.WriteError(w, http.StatusRequestEntityTooLarge, "payload_too_large", err.Error())
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "invalid form body")
		return
	}
	var in interaction
	if err := json.Unmarshal([]byte(form.Get("payload")), &in); err != nil {
		forgeutil.WriteError(w, http.StatusBadRequest, "invalid_payload", "invalid interaction payload")
		return
	}
	ctx := r.Context()
	integ, ierr := h.findIntegration(ctx, in.APIAppID, in.Team.ID)
	if !h.verify(ctx, r, body, integ) {
		forgeutil.WriteError(w, http.StatusUnauthorized, "invalid_signature", "Slack request signature verification failed")
		return
	}
	if ierr != nil || in.Type != "block_actions" || len(in.Actions) == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}
	act := in.Actions[0]
	if !strings.HasPrefix(act.ActionID, ActionPrefix) {
		w.WriteHeader(http.StatusOK)
		return
	}
	var v actionValue
	if err := json.Unmarshal([]byte(act.Value), &v); err != nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	team := integ.TeamID
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		h.handleAction(bctx, team, in, v)
	}()
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) respond(ctx context.Context, url string, body map[string]any) {
	if url == "" {
		return
	}
	if err := RespondURL(ctx, h.opts.HTTPClient, url, body); err != nil {
		h.log.Warn("slack response_url", "err", err)
	}
}

func (h *Handler) handleAction(ctx context.Context, teamID string, in interaction, v actionValue) {
	member, err := integrations.Member(ctx, h.opts.Store, teamID, Provider, in.User.ID)
	if err != nil {
		h.log.Warn("membership lookup", "err", err)
	}
	if member == nil {
		msg := "Only linked members of this factory's team can answer. Link your Slack account and try again."
		if u := h.opts.Linker.PrivateURL(teamID, Provider, in.User.ID, in.User.Username); u != "" {
			msg = "Only linked members of this factory's team can answer. <" + u + "|Link your Slack account> and try again."
		}
		h.respond(ctx, in.ResponseURL, map[string]any{"response_type": "ephemeral", "replace_original": false, "text": msg})
		return
	}
	inboxID := v.InboxID
	if inboxID == "" && v.WorkItemID != "" && h.opts.Store != nil {
		it, err := integrations.OpenInboxItem(ctx, h.opts.Store, v.WorkItemID)
		if err != nil {
			h.respond(ctx, in.ResponseURL, map[string]any{"response_type": "ephemeral", "replace_original": false,
				"text": "This request has already been answered or is no longer waiting for input."})
			return
		}
		inboxID = it.ID
	}
	if h.opts.OnAction == nil || inboxID == "" {
		h.log.Warn("slack action received but no inbox resolver configured")
		return
	}
	if err := h.opts.OnAction(ctx, inboxID, v.Option, member.ID); err != nil {
		h.log.Warn("resolve inbox from slack", "inbox", inboxID, "err", err)
		h.respond(ctx, in.ResponseURL, map[string]any{"response_type": "ephemeral", "replace_original": false,
			"text": "Your answer could not be recorded: " + err.Error()})
		return
	}
	// Replace the buttons with the decision.
	blocks := make([]map[string]any, 0, len(in.Message.Blocks)+1)
	for _, b := range in.Message.Blocks {
		if b["type"] == "actions" {
			continue
		}
		blocks = append(blocks, b)
	}
	blocks = append(blocks, map[string]any{
		"type":     "context",
		"elements": []map[string]any{{"type": "mrkdwn", "text": fmt.Sprintf("*%s* selected by <@%s>", escape(v.Option), in.User.ID)}},
	})
	h.respond(ctx, in.ResponseURL, map[string]any{"replace_original": true, "text": in.Message.Text, "blocks": blocks})
}
