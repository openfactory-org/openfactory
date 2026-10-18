package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// ActionPrefix prefixes the action_id of option buttons.
const ActionPrefix = "of_option_"

// Block Kit limits.
const (
	maxSectionText = 3000
	maxButtonText  = 75
	maxButtons     = 25
)

// ResponderOptions configures the responder.
type ResponderOptions struct {
	Store      core.Store
	Secrets    core.SecretStore
	APIBaseURL string
	HTTPClient *http.Client
	// FactoryOf returns the factory of a work item. A reply uses it when the
	// message has no factory, to select the record of the factory. Optional.
	FactoryOf func(ctx context.Context, workItemID string) (string, error)
	Logger    *slog.Logger
}

// Responder posts outbound messages into the Slack thread where work started.
type Responder struct{ opts ResponderOptions }

var _ core.Responder = (*Responder)(nil)

// NewResponder creates the responder.
func NewResponder(opts ResponderOptions) *Responder { return &Responder{opts: opts} }

// Provider implements core.Responder.
func (r *Responder) Provider() string { return Provider }

// integration selects the record for a reply with the selection rule:
// the records of the team whose team_id is the
// workspace of the source, the record of the factory first, then the team
// record. factoryID "" falls back to FactoryOf of the work item.
func (r *Responder) integration(ctx context.Context, teamID, factoryID, workItemID string, src core.SourceRef) (*core.Integration, error) {
	if factoryID == "" && r.opts.FactoryOf != nil && workItemID != "" {
		if fid, err := r.opts.FactoryOf(ctx, workItemID); err == nil {
			factoryID = fid
		}
	}
	in, err := integrations.SelectTeam(ctx, r.opts.Store, teamID, Provider, factoryID, src.Workspace)
	if err != nil {
		return nil, err
	}
	return &in, nil
}

// Reply implements core.Responder.
func (r *Responder) Reply(ctx context.Context, teamID string, msg core.OutboundMessage) error {
	if msg.Source.Channel == "" {
		return errors.New("slack: outbound message has no channel")
	}
	in, err := r.integration(ctx, teamID, msg.FactoryID, msg.WorkItemID, msg.Source)
	if err != nil {
		return err
	}
	tok, err := forgeutil.IntegrationSecret(ctx, r.opts.Secrets, *in, "")
	if err != nil {
		return err
	}
	c := NewClient(r.opts.APIBaseURL, string(tok), r.opts.HTTPClient)
	_, err = c.PostMessage(ctx, BuildMessage(msg))
	return err
}

// BuildMessage renders an outbound message as a threaded Block Kit message with option buttons.
func BuildMessage(msg core.OutboundMessage) Message {
	text := Mrkdwn(msg.Text)
	switch msg.Kind {
	case "approval_request":
		text = ":raised_hand: *Approval needed*\n" + text
	case "question":
		text = ":question: *Question*\n" + text
	case "error":
		text = ":warning: " + text
	}
	var blocks []Block
	for _, chunk := range chunks(text, maxSectionText) {
		blocks = append(blocks, Block{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": chunk}})
	}
	if len(msg.Options) > 0 {
		var elems []map[string]any
		for i, opt := range msg.Options {
			if i >= maxButtons {
				break
			}
			val, _ := json.Marshal(actionValue{WorkItemID: msg.WorkItemID, InboxID: msg.InboxID, Option: opt})
			btn := map[string]any{
				"type":      "button",
				"action_id": fmt.Sprintf("%s%d", ActionPrefix, i),
				"text":      map[string]any{"type": "plain_text", "text": forgeutil.Truncate(opt, maxButtonText), "emoji": true},
				"value":     string(val),
			}
			if i == 0 && msg.Kind == "approval_request" {
				btn["style"] = "primary"
			}
			elems = append(elems, btn)
		}
		blocks = append(blocks, Block{"type": "actions", "block_id": "of_options", "elements": elems})
	}
	return Message{
		Channel:  msg.Source.Channel,
		ThreadTS: msg.Source.ThreadID,
		Text:     forgeutil.Truncate(plain(msg.Text), 3000),
		Blocks:   blocks,
	}
}

func chunks(s string, n int) []string {
	r := []rune(s)
	if len(r) == 0 {
		return []string{" "}
	}
	var out []string
	for len(r) > n {
		cut := n
		for i := n; i > n/2; i-- {
			if r[i] == '\n' {
				cut = i
				break
			}
		}
		out = append(out, string(r[:cut]))
		r = r[cut:]
	}
	return append(out, string(r))
}

var (
	mdLink    = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)\s]+)\)`)
	mdBold    = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	mdHeading = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	mdStrike  = regexp.MustCompile(`~~([^~\n]+)~~`)
)

func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// Mrkdwn converts common Markdown to Slack mrkdwn (links, bold, headings, strikethrough).
func Mrkdwn(md string) string {
	s := escape(md)
	// links were escaped too; match on the escaped text
	s = mdLink.ReplaceAllString(s, "<$2|$1>")
	s = mdHeading.ReplaceAllString(s, "*$1*")
	s = mdBold.ReplaceAllString(s, "*$1*")
	s = mdStrike.ReplaceAllString(s, "~$1~")
	return s
}

func plain(md string) string {
	s := mdLink.ReplaceAllString(md, "$1 ($2)")
	s = mdBold.ReplaceAllString(s, "$1")
	return mdHeading.ReplaceAllString(s, "$1")
}
