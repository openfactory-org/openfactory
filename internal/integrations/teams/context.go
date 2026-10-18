package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
	"github.com/openfactory-org/openfactory/internal/integrations"
)

// Limits of the event context of a thread.
const (
	maxThreadMessages = 200
	graphPageSize     = 50
)

var _ eventctx.Builder = (*Handler)(nil)

// ThreadAttachment is a file that a message of the thread has. The builder
// lists the files by name. It does not download them.
type ThreadAttachment struct {
	Name        string `json:"name,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

// ThreadEntry is one message of context/thread.json.
type ThreadEntry struct {
	ID          string             `json:"id"`
	Time        string             `json:"time,omitempty"`
	User        string             `json:"user,omitempty"`
	UserID      string             `json:"user_id,omitempty"`
	Bot         bool               `json:"bot,omitempty"`
	Text        string             `json:"text"`
	Attachments []ThreadAttachment `json:"attachments,omitempty"`
}

// EventContext implements eventctx.Builder: the messages of the thread of the
// event, the first message and its replies, in time order
// (context/thread.json). It reads them with Microsoft Graph, as the bot of
// the integration record. The bot needs the resource permission
// ChannelMessage.Read.Group in the team, and the record needs tenant_id.
//
// The file has at most eventctx.MaxPayloadBytes. When the thread is longer,
// the first message and the newest replies stay, and the omitted list says
// how many messages were left out. If Graph does not return the thread, the
// omitted list says why, and the run goes on. The component that writes the
// files into the workspace redacts the secret values (eventctx.Redact).
func (h *Handler) EventContext(ctx context.Context, ev *core.Event) ([]eventctx.File, []eventctx.Omitted, error) {
	channel, root := ev.Source.Channel, ev.Source.ThreadID
	if channel == "" || root == "" {
		return nil, nil, nil
	}
	graphTeam, _ := ev.Attributes[eventattr.TeamID].(string)
	tenant, _ := ev.Attributes[eventattr.TenantID].(string)
	if graphTeam == "" {
		return nil, nil, errors.New("teams: the event has no team ID")
	}
	sel, err := integrations.SelectTeam(ctx, h.opts.Store, ev.TeamID, IntegrationProvider, ev.FactoryID, graphTeam)
	if err != nil {
		return nil, nil, err
	}
	in := &sel
	omit := func(reason string) ([]eventctx.File, []eventctx.Omitted, error) {
		return nil, []eventctx.Omitted{{Name: "context/thread.json", Reason: reason}}, nil
	}
	c, err := h.graphClient(ctx, in, tenant)
	if err != nil {
		return omit(err.Error())
	}
	const perm = "ChannelMessage.Read.Group"
	base := "/teams/" + url.PathEscape(graphTeam) + "/channels/" + url.PathEscape(channel) + "/messages/" + url.PathEscape(root)
	var first map[string]any
	if err := c.Do(ctx, "GET", base, nil, &first); err != nil {
		return omit(describeGraphError("read the thread", perm, err).Error())
	}
	msgs := []map[string]any{first}
	err = h.graphPages(ctx, c, base+"/replies?$top="+fmt.Sprint(graphPageSize), func(items []map[string]any) bool {
		msgs = append(msgs, items...)
		return len(msgs) < maxThreadMessages*2
	})
	if err != nil {
		return omit(describeGraphError("read the replies", perm, err).Error())
	}
	entries := make([]ThreadEntry, 0, len(msgs))
	for _, m := range msgs {
		if e, ok := threadEntry(m); ok {
			entries = append(entries, e)
		}
	}
	// Graph does not promise an order: the oldest message comes first.
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Time < entries[j].Time })
	data, left := fitThread(entries)
	var omitted []eventctx.Omitted
	if left > 0 {
		omitted = append(omitted, eventctx.Omitted{Name: "context/thread.json",
			Reason: fmt.Sprintf("%d of %d messages were left out: the thread is longer than the limit of %d messages or %d KiB", left, len(entries), maxThreadMessages, eventctx.MaxPayloadBytes>>10)})
	}
	return []eventctx.File{{Path: "thread.json", Data: data}}, omitted, nil
}

// threadEntry converts a Graph chatMessage. It leaves out the system
// messages and the deleted messages.
func threadEntry(m map[string]any) (ThreadEntry, bool) {
	if t := str(m, "messageType"); t != "" && t != "message" {
		return ThreadEntry{}, false
	}
	if str(m, "deletedDateTime") != "" {
		return ThreadEntry{}, false
	}
	e := ThreadEntry{ID: str(m, "id"), Time: str(m, "createdDateTime")}
	if from := obj(m, "from"); from != nil {
		if u := obj(from, "user"); u != nil {
			e.User, e.UserID = str(u, "displayName"), str(u, "id")
		} else if a := obj(from, "application"); a != nil {
			e.User, e.Bot = str(a, "displayName"), true
		}
	}
	if b := obj(m, "body"); b != nil {
		e.Text = str(b, "content")
		if strings.EqualFold(str(b, "contentType"), "html") {
			e.Text = CleanText(e.Text)
		}
	}
	if list, ok := m["attachments"].([]any); ok {
		for _, a := range list {
			if am, ok := a.(map[string]any); ok && str(am, "name") != "" {
				e.Attachments = append(e.Attachments, ThreadAttachment{Name: str(am, "name"), ContentType: str(am, "contentType")})
			}
		}
	}
	return e, e.ID != ""
}

// fitThread writes the entries as JSON within the limits. When the thread is
// too long, the first message stays, the oldest replies go first, and left
// is the number of messages that were dropped.
func fitThread(entries []ThreadEntry) (data []byte, left int) {
	keep := entries
	if len(keep) > maxThreadMessages {
		keep = append([]ThreadEntry{keep[0]}, keep[len(keep)-maxThreadMessages+1:]...)
	}
	for {
		b, _ := json.MarshalIndent(keep, "", "  ")
		if len(b) <= eventctx.MaxPayloadBytes || len(keep) <= 1 {
			return b, len(entries) - len(keep)
		}
		// Drop the oldest reply, in steps that grow with the size.
		drop := 1 + len(keep)/20
		if drop > len(keep)-1 {
			drop = len(keep) - 1
		}
		keep = append([]ThreadEntry{keep[0]}, keep[1+drop:]...)
	}
}
