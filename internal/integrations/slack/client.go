// Package slack connects factories to Slack: the Events API receiver (URL
// verification, v0 request signing, mentions, messages, DMs, reactions and
// member joins), interactive actions (approval buttons), account linking, and
// a core.Responder that posts into the originating thread.
package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// DefaultAPIBaseURL is the Slack Web API root.
const DefaultAPIBaseURL = "https://slack.com/api"

// APIError is a Slack Web API error ({"ok":false,"error":"..."}).
type APIError struct {
	Method string
	Code   string
}

func (e *APIError) Error() string { return "slack " + e.Method + ": " + e.Code }

// Client is a minimal Slack Web API client authenticated with a bot token.
type Client struct {
	api *forgeutil.Client
}

// NewClient creates a Web API client.
func NewClient(baseURL, token string, hc *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultAPIBaseURL
	}
	return &Client{api: &forgeutil.Client{
		BaseURL:   baseURL,
		HTTP:      hc,
		UserAgent: "openfactory-slack",
		Authorize: func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+token)
			return nil
		},
	}}
}

// Call invokes a Web API method with a JSON body (or form values for read methods) and decodes the response.
func (c *Client) Call(ctx context.Context, method string, in any, out any) error {
	var raw json.RawMessage
	var err error
	if v, ok := in.(url.Values); ok {
		err = c.api.Do(ctx, http.MethodGet, method+"?"+v.Encode(), nil, &raw)
	} else {
		err = c.api.Do(ctx, http.MethodPost, method, in, &raw)
	}
	if err != nil {
		return err
	}
	var status struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return fmt.Errorf("slack %s: decode: %w", method, err)
	}
	if !status.OK {
		return &APIError{Method: method, Code: status.Error}
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// Message is a chat.postMessage request.
type Message struct {
	Channel  string  `json:"channel"`
	Text     string  `json:"text"`
	ThreadTS string  `json:"thread_ts,omitempty"`
	Blocks   []Block `json:"blocks,omitempty"`
	User     string  `json:"user,omitempty"` // chat.postEphemeral only
	Mrkdwn   bool    `json:"mrkdwn,omitempty"`
}

// Block is a Block Kit block (kept generic).
type Block = map[string]any

// PostMessage posts a message and returns its ts.
func (c *Client) PostMessage(ctx context.Context, m Message) (string, error) {
	var out struct {
		TS string `json:"ts"`
	}
	m.User = ""
	if err := c.Call(ctx, "chat.postMessage", m, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// PostEphemeral shows a message only to user.
func (c *Client) PostEphemeral(ctx context.Context, m Message) error {
	return c.Call(ctx, "chat.postEphemeral", m, nil)
}

// AddReaction adds an emoji reaction to a message (already_reacted is not an error).
func (c *Client) AddReaction(ctx context.Context, channel, ts, name string) error {
	err := c.Call(ctx, "reactions.add", map[string]string{"channel": channel, "timestamp": ts, "name": name}, nil)
	var ae *APIError
	if err != nil && asAPIError(err, &ae) && ae.Code == "already_reacted" {
		return nil
	}
	return err
}

func asAPIError(err error, target **APIError) bool {
	return errors.As(err, target)
}

// UserInfo is the subset of users.info used for filters and account matching.
type UserInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsBot   bool   `json:"is_bot"`
	Deleted bool   `json:"deleted"`
	// TeamID is the workspace of the user. IsRestricted marks a
	// multi-channel guest, IsUltraRestricted a single-channel guest, and
	// IsStranger a user of another workspace (a shared channel).
	TeamID            string `json:"team_id"`
	IsRestricted      bool   `json:"is_restricted"`
	IsUltraRestricted bool   `json:"is_ultra_restricted"`
	IsStranger        bool   `json:"is_stranger"`
	Profile           struct {
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
	} `json:"profile"`
}

// UserInfo looks up a user.
func (c *Client) UserInfo(ctx context.Context, userID string) (*UserInfo, error) {
	var out struct {
		User UserInfo `json:"user"`
	}
	if err := c.Call(ctx, "users.info", url.Values{"user": {userID}}, &out); err != nil {
		return nil, err
	}
	return &out.User, nil
}

// ChannelName returns a conversation's name.
func (c *Client) ChannelName(ctx context.Context, channel string) (string, error) {
	var out struct {
		Channel struct {
			Name string `json:"name"`
		} `json:"channel"`
	}
	if err := c.Call(ctx, "conversations.info", url.Values{"channel": {channel}}, &out); err != nil {
		return "", err
	}
	return out.Channel.Name, nil
}

// HistoryMessage is a message returned by conversations.history / replies.
type HistoryMessage struct {
	TS          string            `json:"ts"`
	ThreadTS    string            `json:"thread_ts"`
	User        string            `json:"user"`
	BotID       string            `json:"bot_id"`
	Text        string            `json:"text"`
	Blocks      []json.RawMessage `json:"blocks"`
	Attachments []json.RawMessage `json:"attachments"`
}

// GetMessage fetches one message by channel and ts (root or thread reply).
func (c *Client) GetMessage(ctx context.Context, channel, ts string) (*HistoryMessage, error) {
	var out struct {
		Messages []HistoryMessage `json:"messages"`
	}
	err := c.Call(ctx, "conversations.history", url.Values{
		"channel": {channel}, "latest": {ts}, "oldest": {ts}, "inclusive": {"true"}, "limit": {"1"},
	}, &out)
	if err == nil && len(out.Messages) > 0 && out.Messages[0].TS == ts {
		return &out.Messages[0], nil
	}
	// Thread replies are not returned by history; ask conversations.replies.
	out.Messages = nil
	if err := c.Call(ctx, "conversations.replies", url.Values{
		"channel": {channel}, "ts": {ts}, "latest": {ts}, "oldest": {ts}, "inclusive": {"true"}, "limit": {"1"},
	}, &out); err != nil {
		return nil, err
	}
	for i := range out.Messages {
		if out.Messages[i].TS == ts {
			return &out.Messages[i], nil
		}
	}
	return nil, fmt.Errorf("slack: message %s not found in %s", ts, channel)
}

// RespondURL posts to an interaction response_url.
func RespondURL(ctx context.Context, hc *http.Client, responseURL string, body any) error {
	c := &forgeutil.Client{HTTP: hc, UserAgent: "openfactory-slack"}
	return c.Do(ctx, http.MethodPost, responseURL, body, nil)
}

// TextOfBlocks extracts visible text from Block Kit blocks and legacy attachments.
func TextOfBlocks(blocks, attachments []json.RawMessage) string {
	var parts []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if s, ok := t["text"].(string); ok && s != "" {
				parts = append(parts, s)
			}
			for k, child := range t {
				if k == "text" {
					if _, isStr := child.(string); isStr {
						continue
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	for _, raw := range append(append([]json.RawMessage{}, blocks...), attachments...) {
		var v any
		if json.Unmarshal(raw, &v) == nil {
			if m, ok := v.(map[string]any); ok {
				for _, k := range []string{"pretext", "title", "fallback"} {
					if s, ok := m[k].(string); ok && s != "" {
						parts = append(parts, s)
					}
				}
			}
			walk(v)
		}
	}
	return strings.Join(parts, "\n")
}
