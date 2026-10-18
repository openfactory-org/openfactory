package core

import (
	"context"
	"strings"
	"time"
)

// Actor kinds of a pull request push (PullRequestPush.Kind).
const (
	PushByAgent  = "agent"  // the product's own bot or app
	PushByBot    = "bot"    // another bot
	PushByPerson = "person" // a person
)

// PullRequestFacts are the facts about one pull request that the metrics
// need. The engine updates them from forge events: the opener, each
// push with the kind of its actor, the first review, the merge and the size.
// The table is pull_request_facts. Facts exist only for pull requests whose
// events arrived after the forge was connected.
type PullRequestFacts struct {
	// ID is TeamID + "|" + Key.
	ID     string `json:"id"`
	TeamID string `json:"team_id"`
	// Key is the source key of the pull request (SourceRef.Key).
	Key        string `json:"key"`
	Provider   string `json:"provider"`
	Repo       string `json:"repo"`
	Number     int    `json:"number"`
	URL        string `json:"url,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`

	Opener     string     `json:"opener,omitempty"`
	OpenerKind string     `json:"opener_kind,omitempty"` // PushByAgent, PushByBot or PushByPerson
	OpenedAt   *time.Time `json:"opened_at,omitempty"`
	// Pushes are the pushes to the head branch after the pull request opened.
	Pushes        []PullRequestPush `json:"pushes,omitempty"`
	FirstReviewAt *time.Time        `json:"first_review_at,omitempty"`
	MergedAt      *time.Time        `json:"merged_at,omitempty"`
	ClosedAt      *time.Time        `json:"closed_at,omitempty"`
	// Additions and Deletions are the changed lines. They are nil when the
	// forge does not send them.
	Additions *int `json:"additions,omitempty"`
	Deletions *int `json:"deletions,omitempty"`

	UpdatedAt time.Time `json:"updated_at"`
}

// PullRequestPush is one push to a pull request.
type PullRequestPush struct {
	At    time.Time `json:"at"`
	Actor string    `json:"actor,omitempty"`
	Kind  string    `json:"kind"` // PushByAgent, PushByBot or PushByPerson
	SHA   string    `json:"sha,omitempty"`
}

// PullRequestFactsID returns the ID of the facts of a pull request.
func PullRequestFactsID(teamID, key string) string { return teamID + "|" + key }

// Open reports whether the pull request is neither merged nor closed.
func (f *PullRequestFacts) Open() bool { return f.MergedAt == nil && f.ClosedAt == nil }

// ChangedLines returns additions plus deletions, and false when the size is
// not known.
func (f *PullRequestFacts) ChangedLines() (int, bool) {
	if f.Additions == nil && f.Deletions == nil {
		return 0, false
	}
	n := 0
	if f.Additions != nil {
		n += *f.Additions
	}
	if f.Deletions != nil {
		n += *f.Deletions
	}
	return n, true
}

// PersonPushedBefore reports whether a person opened the pull request, or
// pushed to it, before t.
func (f *PullRequestFacts) PersonPushedBefore(t time.Time) bool {
	if f.OpenerKind == PushByPerson {
		return true
	}
	for _, p := range f.Pushes {
		if p.Kind == PushByPerson && !p.At.After(t) {
			return true
		}
	}
	return false
}

// PullRequestFactFilter selects pull request facts.
type PullRequestFactFilter struct {
	TeamID     string
	Keys       []string // empty: all keys
	Provider   string
	Repo       string // case-insensitive
	HeadBranch string
	OpenOnly   bool
}

// Match reports whether f matches the filter.
func (q PullRequestFactFilter) Match(f *PullRequestFacts) bool {
	if q.TeamID != "" && f.TeamID != q.TeamID {
		return false
	}
	if len(q.Keys) > 0 {
		ok := false
		for _, k := range q.Keys {
			if k == f.Key {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	if q.Provider != "" && !strings.EqualFold(q.Provider, f.Provider) {
		return false
	}
	if q.Repo != "" && !strings.EqualFold(q.Repo, f.Repo) {
		return false
	}
	if q.HeadBranch != "" && q.HeadBranch != f.HeadBranch {
		return false
	}
	return !q.OpenOnly || f.Open()
}

// PullRequestFactStore is an optional Store capability: the table
// pull_request_facts. A store without it records no facts, and the metrics
// fall back to the work item figures.
type PullRequestFactStore interface {
	// PutPullRequestFacts inserts or replaces the facts with f.ID.
	PutPullRequestFacts(ctx context.Context, f *PullRequestFacts) error
	// GetPullRequestFacts returns the facts with the ID, or ErrNotFound.
	GetPullRequestFacts(ctx context.Context, id string) (*PullRequestFacts, error)
	// ListPullRequestFacts returns the facts that match the filter.
	ListPullRequestFacts(ctx context.Context, f PullRequestFactFilter) ([]PullRequestFacts, error)
}
