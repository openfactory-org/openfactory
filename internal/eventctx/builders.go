package eventctx

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// defaultBuilders read the context that the payload already has. Builders
// that call a provider API (chat thread history, files) are registered by
// the integrations.
func defaultBuilders() []Builder {
	return []Builder{
		payloadBuilder{provider: "linear", build: linearIssue},
		payloadBuilder{provider: "jira", build: jiraIssue},
		payloadBuilder{provider: "github", build: githubReviewComment},
		payloadBuilder{provider: "gitlab", build: gitlabNote},
	}
}

type payloadBuilder struct {
	provider string
	build    func(ev *core.Event) []File
}

func (p payloadBuilder) Provider() string { return p.provider }

func (p payloadBuilder) EventContext(_ context.Context, ev *core.Event) ([]File, []Omitted, error) {
	if len(ev.Payload) == 0 {
		return nil, nil, nil
	}
	return p.build(ev), nil, nil
}

// Issue is context/issue.json of a tracker event.
type Issue struct {
	Key         string   `json:"key,omitempty"`
	Title       string   `json:"title,omitempty"`
	Description string   `json:"description,omitempty"`
	State       string   `json:"state,omitempty"`
	Labels      []string `json:"labels,omitempty"`
	Assignee    string   `json:"assignee,omitempty"`
	Project     string   `json:"project,omitempty"`
	URL         string   `json:"url,omitempty"`
}

func issueFile(is Issue) []File {
	if is.Key == "" && is.Title == "" && is.Description == "" {
		return nil
	}
	b, err := json.MarshalIndent(is, "", "  ")
	if err != nil {
		return nil
	}
	return []File{{Path: "issue.json", Data: b}}
}

type named struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
}

func (n *named) String() string {
	if n == nil {
		return ""
	}
	if n.Name != "" {
		return n.Name
	}
	return n.DisplayName
}

func linearIssue(ev *core.Event) []File {
	var p struct {
		Type string `json:"type"`
		Data struct {
			Identifier  string  `json:"identifier"`
			Title       string  `json:"title"`
			Description string  `json:"description"`
			URL         string  `json:"url"`
			State       *named  `json:"state"`
			Assignee    *named  `json:"assignee"`
			Project     *named  `json:"project"`
			Labels      []named `json:"labels"`
			Issue       *struct {
				Identifier string `json:"identifier"`
				Title      string `json:"title"`
				URL        string `json:"url"`
			} `json:"issue"`
		} `json:"data"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil {
		return nil
	}
	d := p.Data
	is := Issue{Key: d.Identifier, Title: d.Title, Description: d.Description, URL: d.URL,
		State: d.State.String(), Assignee: d.Assignee.String(), Project: d.Project.String()}
	if d.Issue != nil && is.Key == "" { // a comment event names its issue
		is.Key, is.Title, is.URL = d.Issue.Identifier, d.Issue.Title, d.Issue.URL
	}
	for _, l := range d.Labels {
		is.Labels = append(is.Labels, l.Name)
	}
	return issueFile(is)
}

func jiraIssue(ev *core.Event) []File {
	var p struct {
		Issue *struct {
			Key    string `json:"key"`
			Self   string `json:"self"`
			Fields struct {
				Summary     string          `json:"summary"`
				Description json.RawMessage `json:"description"`
				Status      *named          `json:"status"`
				Assignee    *named          `json:"assignee"`
				Project     *named          `json:"project"`
				Labels      []string        `json:"labels"`
			} `json:"fields"`
		} `json:"issue"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil || p.Issue == nil {
		return nil
	}
	f := p.Issue.Fields
	is := Issue{Key: p.Issue.Key, Title: f.Summary, State: f.Status.String(), Assignee: f.Assignee.String(),
		Project: f.Project.String(), Labels: f.Labels, URL: ev.Source.URL}
	var s string
	if json.Unmarshal(f.Description, &s) == nil {
		is.Description = s
	} else if len(f.Description) > 0 && string(f.Description) != "null" {
		is.Description = strings.TrimSpace(textOfDoc(f.Description))
	}
	return issueFile(is)
}

// textOfDoc reads the text nodes of a rich-text document (a tree of nodes
// with "text" and "content").
func textOfDoc(raw json.RawMessage) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if s, ok := t["text"].(string); ok {
				b.WriteString(s)
			}
			if c, ok := t["content"]; ok {
				walk(c)
			}
			if typ, _ := t["type"].(string); typ == "paragraph" || typ == "heading" || typ == "listItem" {
				b.WriteString("\n")
			}
		case []any:
			for _, c := range t {
				walk(c)
			}
		}
	}
	walk(v)
	return b.String()
}

// ReviewComment is context/review_comment.json of a review comment event.
type ReviewComment struct {
	Author    string `json:"author,omitempty"`
	Body      string `json:"body,omitempty"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	CommitID  string `json:"commit_id,omitempty"`
	InReplyTo int64  `json:"in_reply_to_id,omitempty"`
	URL       string `json:"url,omitempty"`
}

func reviewFiles(rc ReviewComment, hunk string) []File {
	var out []File
	if b, err := json.MarshalIndent(rc, "", "  "); err == nil {
		out = append(out, File{Path: "review_comment.json", Data: b})
	}
	if strings.TrimSpace(hunk) != "" {
		out = append(out, File{Path: "diff.patch", Data: []byte("--- a/" + rc.Path + "\n+++ b/" + rc.Path + "\n" + hunk + "\n")})
	}
	return out
}

func githubReviewComment(ev *core.Event) []File {
	var p struct {
		Comment *struct {
			Body      string `json:"body"`
			Path      string `json:"path"`
			DiffHunk  string `json:"diff_hunk"`
			Line      int    `json:"line"`
			CommitID  string `json:"commit_id"`
			InReplyTo int64  `json:"in_reply_to_id"`
			HTMLURL   string `json:"html_url"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"comment"`
	}
	// A comment on a line of a file has a path. A comment of an issue or of
	// the conversation of a pull request has none.
	if json.Unmarshal(ev.Payload, &p) != nil || p.Comment == nil || (p.Comment.Path == "" && p.Comment.DiffHunk == "") {
		return nil
	}
	c := p.Comment
	return reviewFiles(ReviewComment{Author: c.User.Login, Body: c.Body, Path: c.Path, Line: c.Line, CommitID: c.CommitID,
		InReplyTo: c.InReplyTo, URL: c.HTMLURL}, c.DiffHunk)
}

func gitlabNote(ev *core.Event) []File {
	var p struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		ObjectAttributes *struct {
			Note         string `json:"note"`
			URL          string `json:"url"`
			CommitID     string `json:"commit_id"`
			DiscussionID string `json:"discussion_id"`
			Position     *struct {
				NewPath string `json:"new_path"`
				NewLine int    `json:"new_line"`
			} `json:"position"`
		} `json:"object_attributes"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil || p.ObjectAttributes == nil || p.ObjectAttributes.Position == nil {
		return nil
	}
	a := p.ObjectAttributes
	return reviewFiles(ReviewComment{Author: p.User.Username, Body: a.Note, Path: a.Position.NewPath, Line: a.Position.NewLine,
		CommitID: a.CommitID, URL: a.URL}, "")
}
