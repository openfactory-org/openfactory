package sqlstore

import (
	"context"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Pull request facts (core.PullRequestFactStore).

var _ core.PullRequestFactStore = (*Store)(nil)

func prFactsRow(f *core.PullRequestFacts) (*row, error) {
	r, err := docRow(f.ID, f)
	if err != nil {
		return nil, err
	}
	open := 0
	if f.Open() {
		open = 1
	}
	r.set("team_id", f.TeamID).set("pr_key", f.Key).set("repo", strings.ToLower(f.Repo)).
		set("head_branch", f.HeadBranch).set("is_open", open)
	return r, nil
}

// PutPullRequestFacts implements core.PullRequestFactStore.
func (s *Store) PutPullRequestFacts(ctx context.Context, f *core.PullRequestFacts) error {
	if f.ID == "" {
		f.ID = core.PullRequestFactsID(f.TeamID, f.Key)
	}
	r, err := prFactsRow(f)
	if err != nil {
		return err
	}
	return s.upsert(ctx, s.conn(ctx), "pull_request_facts", []string{"id"}, r)
}

// GetPullRequestFacts implements core.PullRequestFactStore.
func (s *Store) GetPullRequestFacts(ctx context.Context, id string) (*core.PullRequestFacts, error) {
	return getDoc[core.PullRequestFacts](ctx, s, s.conn(ctx), "SELECT data FROM pull_request_facts WHERE id=?", id)
}

// ListPullRequestFacts implements core.PullRequestFactStore.
func (s *Store) ListPullRequestFacts(ctx context.Context, q core.PullRequestFactFilter) ([]core.PullRequestFacts, error) {
	w := &where{}
	if q.TeamID != "" {
		w.add("team_id=?", q.TeamID)
	}
	if len(q.Keys) > 0 {
		w.in("pr_key", q.Keys)
	}
	if q.Repo != "" {
		w.add("repo=?", strings.ToLower(q.Repo))
	}
	if q.HeadBranch != "" {
		w.add("head_branch=?", q.HeadBranch)
	}
	if q.OpenOnly {
		w.add("is_open=1")
	}
	all, err := listDocs[core.PullRequestFacts](ctx, s, s.conn(ctx), "SELECT data FROM pull_request_facts"+w.sql()+" ORDER BY id", w.args...)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for i := range all {
		if q.Match(&all[i]) {
			out = append(out, all[i])
		}
	}
	return out, nil
}
