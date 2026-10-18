package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Per-work-item execution state set by DispatchRequest (benchmark trials) and
// applied to every run of the work item, including orchestrated children.

func repoRefsKey(workItemID string) string  { return "engine:wi:" + workItemID + ":repo_refs" }
func agentPinsKey(workItemID string) string { return "engine:wi:" + workItemID + ":agent_pins" }

func kvPutJSON(ctx context.Context, st core.Store, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return st.KVSet(ctx, key, string(b))
}

func kvGetJSON(ctx context.Context, st core.Store, key string, v any) (bool, error) {
	s, err := st.KVGet(ctx, key)
	if errors.Is(err, core.ErrNotFound) || (err == nil && s == "") {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal([]byte(s), v); err != nil {
		return false, fmt.Errorf("decode %s: %w", key, err)
	}
	return true, nil
}

// saveWorkItemState records starting repo refs and agent pins for a work item.
func (e *Engine) saveWorkItemState(ctx context.Context, workItemID string, refs []core.RepoAtCommit, pins map[string]core.AgentOverrides) error {
	if len(refs) > 0 {
		if err := kvPutJSON(ctx, e.store, repoRefsKey(workItemID), refs); err != nil {
			return fmt.Errorf("save starting repo refs: %w", err)
		}
	}
	if len(pins) > 0 {
		if err := kvPutJSON(ctx, e.store, agentPinsKey(workItemID), pins); err != nil {
			return fmt.Errorf("save agent pins: %w", err)
		}
	}
	return nil
}

func loadRepoRefs(ctx context.Context, st core.Store, workItemID string) ([]core.RepoAtCommit, error) {
	if workItemID == "" {
		return nil, nil
	}
	var refs []core.RepoAtCommit
	_, err := kvGetJSON(ctx, st, repoRefsKey(workItemID), &refs)
	return refs, err
}

func loadAgentPins(ctx context.Context, st core.Store, workItemID string) (map[string]core.AgentOverrides, error) {
	if workItemID == "" {
		return nil, nil
	}
	var pins map[string]core.AgentOverrides
	_, err := kvGetJSON(ctx, st, agentPinsKey(workItemID), &pins)
	return pins, err
}

// loadRunRepoRefs returns the starting repo refs of a run: the refs of the run
// itself (a judge run), else the refs of its work item.
func loadRunRepoRefs(ctx context.Context, st core.Store, r *core.Run) ([]core.RepoAtCommit, error) {
	var refs []core.RepoAtCommit
	ok, err := kvGetJSON(ctx, st, core.RunRepoRefsKey(r.ID), &refs)
	if err != nil || ok {
		return refs, err
	}
	return loadRepoRefs(ctx, st, r.WorkItemID)
}

// recordStartCommits stores the repositories of the first session of a run
// with their refs. The runner replaces each ref with the commit that it
// checked out (session.ready). Later sessions keep the first record.
func recordStartCommits(ctx context.Context, st core.Store, runID string, repos []core.RepoCheckout) {
	if len(repos) == 0 {
		return
	}
	if have, err := core.LoadStartCommits(ctx, st, runID); err != nil || have != nil {
		return
	}
	list := make([]core.RepoStart, 0, len(repos))
	for _, c := range repos {
		list = append(list, core.RepoStart{CodeForge: c.Forge, Owner: c.Owner, Repo: c.Name, Path: c.Path, Ref: c.Ref, Resolved: isCommitSHA(c.Ref)})
	}
	_ = core.SaveStartCommits(ctx, st, runID, list)
}

func isCommitSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// applyRepoRefs pins checkouts to the starting commits. Refs naming a
// repository that is not checked out yet are added as extra checkouts.
func applyRepoRefs(repos []core.RepoCheckout, refs []core.RepoAtCommit, cloneURL func(forge, owner, name string) (string, error)) ([]core.RepoCheckout, error) {
	for _, ref := range refs {
		forge := strings.ToUpper(ref.CodeForge)
		found := false
		for i := range repos {
			if strings.EqualFold(repos[i].Forge, forge) && strings.EqualFold(repos[i].Owner, ref.Owner) && strings.EqualFold(repos[i].Name, ref.Repo) {
				repos[i].Ref = ref.Ref
				found = true
			}
		}
		if found {
			continue
		}
		u, err := cloneURL(forge, ref.Owner, ref.Repo)
		if err != nil {
			return repos, fmt.Errorf("starting repository %s/%s: %w", ref.Owner, ref.Repo, err)
		}
		repos = append(repos, core.RepoCheckout{Forge: forge, Owner: ref.Owner, Name: ref.Repo, CloneURL: u, Ref: ref.Ref, Path: ref.Repo})
	}
	return repos, nil
}
