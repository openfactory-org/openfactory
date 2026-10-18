package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil"
)

// Limits of the event context of a thread.
const (
	maxThreadMessages = 200
	maxThreadFiles    = 10
	maxThreadFileSize = 1 << 20
)

// fileHost is the only host that gets the bot token for a file download.
const fileHost = "files.slack.com"

var _ eventctx.Builder = (*Responder)(nil)

type threadFile struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Mimetype           string `json:"mimetype"`
	Size               int64  `json:"size"`
	URLPrivateDownload string `json:"url_private_download"`
}

type threadMessage struct {
	TS       string       `json:"ts"`
	ThreadTS string       `json:"thread_ts"`
	User     string       `json:"user"`
	BotID    string       `json:"bot_id"`
	Text     string       `json:"text"`
	Files    []threadFile `json:"files"`
}

// ThreadEntry is one message of context/thread.json.
type ThreadEntry struct {
	TS    string   `json:"ts"`
	User  string   `json:"user,omitempty"`
	Bot   bool     `json:"bot,omitempty"`
	Text  string   `json:"text"`
	Files []string `json:"files,omitempty"`
}

// EventContext implements eventctx.Builder: the earlier messages of the
// thread (context/thread.json) and the files of the thread that the app can
// read (context/files/<id>-<name>). The app needs the scopes
// channels:history, groups:history, im:history and files:read.
func (r *Responder) EventContext(ctx context.Context, ev *core.Event) ([]eventctx.File, []eventctx.Omitted, error) {
	channel, thread := ev.Source.Channel, ev.Source.ThreadID
	if channel == "" || thread == "" {
		return nil, nil, nil
	}
	in, err := r.integration(ctx, ev.TeamID, ev.FactoryID, "", ev.Source)
	if err != nil {
		return nil, nil, err
	}
	tok, err := forgeutil.IntegrationSecret(ctx, r.opts.Secrets, *in, "")
	if err != nil {
		return nil, nil, fmt.Errorf("slack: integration credential: %w", err)
	}
	c := NewClient(r.opts.APIBaseURL, string(tok), r.opts.HTTPClient)
	var out struct {
		Messages []threadMessage `json:"messages"`
	}
	if err := c.Call(ctx, "conversations.replies", url.Values{
		"channel": {channel}, "ts": {thread}, "limit": {fmt.Sprint(maxThreadMessages)},
	}, &out); err != nil {
		return nil, []eventctx.Omitted{{Name: "context/thread.json", Reason: err.Error()}}, nil //nolint:nilerr // the failure is reported as an omitted context file
	}
	var files []eventctx.File
	var omitted []eventctx.Omitted
	entries := make([]ThreadEntry, 0, len(out.Messages))
	nFiles := 0
	for _, m := range out.Messages {
		e := ThreadEntry{TS: m.TS, User: m.User, Bot: m.BotID != "", Text: m.Text}
		for _, f := range m.Files {
			name := fileName(f)
			e.Files = append(e.Files, "context/files/"+name)
			switch {
			case nFiles >= maxThreadFiles:
				omitted = append(omitted, eventctx.Omitted{Name: f.Name, Reason: "too many files"})
				continue
			case f.Size > maxThreadFileSize:
				omitted = append(omitted, eventctx.Omitted{Name: f.Name, Reason: fmt.Sprintf("larger than %d KiB", maxThreadFileSize>>10)})
				continue
			}
			data, err := r.download(ctx, string(tok), f.URLPrivateDownload)
			if err != nil {
				omitted = append(omitted, eventctx.Omitted{Name: f.Name, Reason: err.Error()})
				continue
			}
			nFiles++
			files = append(files, eventctx.File{Path: "files/" + name, Data: data})
		}
		entries = append(entries, e)
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append([]eventctx.File{{Path: "thread.json", Data: b}}, files...), omitted, nil
}

func fileName(f threadFile) string {
	n := path.Base(strings.ReplaceAll(f.Name, `\`, "/"))
	if n == "." || n == "/" || n == ".." || n == "" {
		n = "file"
	}
	return f.ID + "-" + n
}

// download reads a private file. The token goes only to the file host.
func (r *Responder) download(ctx context.Context, token, raw string) ([]byte, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != fileHost {
		if r.opts.APIBaseURL == "" || !strings.HasPrefix(raw, strings.TrimRight(r.opts.APIBaseURL, "/")+"/") {
			return nil, fmt.Errorf("the file URL is not on %s", fileHost)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	hc := r.opts.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download returned %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxThreadFileSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxThreadFileSize {
		return nil, fmt.Errorf("larger than %d KiB", maxThreadFileSize>>10)
	}
	return data, nil
}
