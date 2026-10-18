package orchestration

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
)

type linkIssueInput struct {
	URL      string `json:"url,omitempty" jsonschema:"Issue URL (Linear, Jira, GitHub, GitLab or an Azure DevOps work item)"`
	Provider string `json:"provider,omitempty" jsonschema:"linear, jira, github, gitlab or azure_devops; derived from the URL when omitted"`
	Key      string `json:"key,omitempty" jsonschema:"Tracker key, e.g. ENG-123; derived from the URL when omitted"`
	Repo     string `json:"repo,omitempty" jsonschema:"owner/name of a forge issue; derived from the URL when omitted"`
	Number   int    `json:"number,omitempty" jsonschema:"Forge issue number; derived from the URL when omitted"`
}

var trackerKeyRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*-[0-9]+$`)

// ParseIssueURL reads the issue of a tracker or forge URL.
func ParseIssueURL(raw string) (core.SourceRef, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return core.SourceRef{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	host := strings.ToLower(u.Hostname())
	src := core.SourceRef{Kind: "issue", URL: raw}
	for i, p := range parts {
		next := ""
		if i+1 < len(parts) {
			next = parts[i+1]
		}
		switch {
		case p == "issue" && strings.Contains(host, "linear") && trackerKeyRe.MatchString(next):
			src.Provider, src.ExternalID = "linear", strings.ToUpper(next)
			return src, true
		case p == "browse" && trackerKeyRe.MatchString(next):
			src.Provider, src.ExternalID = "jira", strings.ToUpper(next)
			return src, true
		case p == "issues" && i >= 2:
			n, err := strconv.Atoi(next)
			if err != nil || n <= 0 {
				continue
			}
			end := i
			provider := "github"
			if parts[end-1] == "-" {
				end--
				provider = "gitlab"
			}
			src.Provider, src.Repo, src.Number = provider, strings.Join(parts[:end], "/"), n
			return src, end >= 2
		case p == "_workitems" && next == "edit" && i+2 < len(parts) && i >= 1:
			n, err := strconv.Atoi(parts[i+2])
			if err != nil || n <= 0 {
				continue
			}
			src.Provider, src.Kind, src.Repo, src.Number = "azure_devops", "work_item", parts[i-1], n
			return src, true
		}
	}
	if q := u.Query().Get("selectedIssue"); trackerKeyRe.MatchString(q) {
		src.Provider, src.ExternalID = "jira", strings.ToUpper(q)
		return src, true
	}
	return core.SourceRef{}, false
}

// issueRef builds the issue reference of a link_issue call.
func issueRef(in linkIssueInput) (core.SourceRef, error) {
	src, parsed := ParseIssueURL(in.URL)
	if in.URL != "" && !parsed && in.Key == "" && in.Number == 0 {
		return core.SourceRef{}, fmt.Errorf("cannot read an issue from url %q; give provider and key (or repo and number)", in.URL)
	}
	if !parsed {
		src = core.SourceRef{Kind: "issue", URL: in.URL}
	}
	if in.Provider != "" {
		src.Provider = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(in.Provider), "-", "_"))
	}
	if in.Key != "" {
		src.ExternalID = strings.TrimSpace(in.Key)
	}
	if in.Repo != "" {
		src.Repo = in.Repo
	}
	if in.Number != 0 {
		src.Number = in.Number
	}
	if src.Provider == "azure_devops" && src.Number != 0 {
		src.Kind = "work_item"
	}
	if src.Provider == "" || src.Key() == "" {
		return core.SourceRef{}, errors.New("url, or provider with key (or repo and number), is required")
	}
	return src, nil
}

func (t *tools) linkIssue(ctx context.Context, _ *mcp.CallToolRequest, in linkIssueInput) (*mcp.CallToolResult, any, error) {
	r, err := t.workItemRun(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	linker, ok := t.s.engine.(core.IssueLinker)
	if !ok {
		return errResult(errors.New("this server cannot link issues")), nil, nil
	}
	issue, err := issueRef(in)
	if err != nil {
		return errResult(err), nil, nil
	}
	if err := linker.LinkIssue(ctx, r.WorkItemID, issue); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Linked issue %s to the work item. Its state now follows the work item.", issue.Key()), nil, nil
}
