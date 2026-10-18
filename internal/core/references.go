package core

import (
	"strings"
	"time"
)

// Kinds of a work item reference.
const (
	RefSource      = "source"       // the action that created the work item
	RefPullRequest = "pull_request" // link_pull_request, or a pull request that the factory opened
	RefIssue       = "issue"        // link_issue: a tracker issue
	RefThread      = "thread"       // a chat thread where the factory posted for the work item
)

// WorkItemRef is one external reference of a work item. Its Key routes
// events to the work item.
type WorkItemRef struct {
	Kind    string    `json:"kind"`
	Key     string    `json:"key"`
	Source  SourceRef `json:"source"`
	AddedAt time.Time `json:"added_at,omitempty"`
}

// Issue states that the engine writes back to a tracker (IssueTracker).
// Each tracker maps them to its own workflow states.
const (
	IssueStateInProgress = "in_progress"
	IssueStateInReview   = "in_review"
	IssueStateDone       = "done"
)

// Outbound message kinds that are not text. The notify router gives them to
// the IssueTracker of the provider of OutboundMessage.Source.
const (
	OutboundIssueState        = "issue_state"         // OutboundMessage.IssueState
	OutboundAttachPullRequest = "attach_pull_request" // OutboundMessage.PullRequest
)

// IsTrackerOperation reports whether an outbound kind is a tracker operation
// and not a text message.
func IsTrackerOperation(kind string) bool {
	return kind == OutboundIssueState || kind == OutboundAttachPullRequest
}

// PullRequestSource returns the source reference of a pull request.
func PullRequestSource(pr PullRequestRef) SourceRef {
	provider := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(pr.Forge), "-", "_"))
	if provider == "" {
		provider = "github"
	}
	return SourceRef{Provider: provider, Kind: "pull_request", URL: pr.URL, Repo: pr.Repo, Number: pr.Number}
}

// Refs returns the references of the work item: the stored references, and
// the source and the pull requests when they are not stored (work items from
// before references existed). The source comes first.
func (w *WorkItem) Refs() []WorkItemRef {
	var out []WorkItemRef
	seen := map[string]bool{}
	add := func(r WorkItemRef) {
		if r.Key == "" {
			r.Key = r.Source.Key()
		}
		id := r.Kind + "|" + r.Key
		if r.Key == "" || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, r)
	}
	if w.Source.Key() != "" {
		add(WorkItemRef{Kind: RefSource, Source: w.Source, AddedAt: w.CreatedAt})
	}
	for _, r := range w.References {
		add(r)
	}
	for _, pr := range w.PullRequests {
		if pr.Repo != "" && pr.Number != 0 {
			add(WorkItemRef{Kind: RefPullRequest, Source: PullRequestSource(pr)})
		}
	}
	return out
}

// HasRef reports whether a reference of the work item has the source key.
func (w *WorkItem) HasRef(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range w.Refs() {
		if r.Key == key {
			return true
		}
	}
	return false
}

// AddRef adds a reference. It reports false when a reference of the same
// kind and key exists.
func (w *WorkItem) AddRef(r WorkItemRef) bool {
	if r.Key == "" {
		r.Key = r.Source.Key()
	}
	if r.Key == "" {
		return false
	}
	for _, x := range w.References {
		if x.Kind == r.Kind && x.Key == r.Key {
			return false
		}
	}
	w.References = append(w.References, r)
	return true
}

// IsTrackerIssue reports whether a source is a tracker issue that the engine
// writes back to: a Linear or Jira issue, or an Azure DevOps work item. A
// GitHub or GitLab issue is not one: the forge closes it from the pull
// request ("Fixes #12").
func (s SourceRef) IsTrackerIssue() bool {
	switch strings.ToLower(s.Provider) {
	case "linear", "jira":
		return s.ExternalID != "" || s.Channel != ""
	case "azure_devops":
		return s.Kind == "work_item"
	}
	return false
}
