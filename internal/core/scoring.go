package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// ---------- Judge runs ----------

// JudgeRunRequest starts the judge of a scorer as a run in a sandbox. The
// scoring service builds it; the engine starts the run.
type JudgeRunRequest struct {
	FactoryID string
	// Name is the label of the run, for example "scorer:tests-run".
	Name string
	// Instructions are the system instructions of the judge (the rubric,
	// the labels and the answer format).
	Instructions string
	// Prompt is the first user message of the judge run.
	Prompt string
	// Harness, Machine, Secrets and MCPServers are the execution settings of
	// the scorer (scorer fields over scorerDefaults over agentDefaults).
	Harness    HarnessConfig
	Machine    string
	Secrets    []string
	MCPServers map[string]MCPServer
	// Files are written to the workspace of the judge, below the skill
	// directory "scored-run" (for example the full transcript).
	Files map[string]string
	// StartingRepoRefs check out the repositories at the start commits of
	// the scored run.
	StartingRepoRefs []RepoAtCommit
	// Metadata identifies what the judge scores.
	Metadata map[string]string
	// TriggeredBy is "scorer:<id>" or "benchmark:<id>".
	TriggeredBy string
}

// JudgeSkillName is the name of the skill that holds the files of a judge run.
const JudgeSkillName = "scored-run"

// ---------- Trial mode ----------

// MetaTrialMode is the run metadata key that puts a run in trial mode. A
// benchmark trial and its child runs carry it (they inherit the metadata).
const MetaTrialMode = "trial_mode"

// IsTrialRun reports whether r runs in trial mode. In trial mode, the write
// tools of the built-in integrations and the outbound replies go to the
// transcript of the run, not to the real services.
func IsTrialRun(r *Run) bool {
	if r == nil {
		return false
	}
	return r.Metadata[MetaTrialMode] == "true" || r.Metadata["benchmark_run"] != ""
}

// ---------- Start commits ----------

// RepoStart is the start commit of one repository of a run.
type RepoStart struct {
	CodeForge string `json:"codeForge"`
	Owner     string `json:"owner"`
	Repo      string `json:"repo"`
	// Path is the directory of the clone in the workspace.
	Path string `json:"path,omitempty"`
	// Ref is the checked-out commit. Before the
	// runner reports the clone, it can be the requested branch or tag.
	Ref string `json:"ref"`
	// Resolved is true when Ref is the commit that the runner checked out.
	Resolved bool `json:"resolved,omitempty"`
}

// RepoAtCommit returns r as a RepoAtCommit.
func (r RepoStart) RepoAtCommit() RepoAtCommit {
	return RepoAtCommit{CodeForge: r.CodeForge, Owner: r.Owner, Repo: r.Repo, Ref: r.Ref}
}

// StartCommitsKey is the key/value store key of the start commits of a run.
func StartCommitsKey(runID string) string { return "run:" + runID + ":start_commits" }

// LoadStartCommits returns the start commits of a run, or nil.
func LoadStartCommits(ctx context.Context, st Store, runID string) ([]RepoStart, error) {
	s, err := st.KVGet(ctx, StartCommitsKey(runID))
	if errors.Is(err, ErrNotFound) || (err == nil && s == "") {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []RepoStart
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode start commits of run %s: %w", runID, err)
	}
	return out, nil
}

// SaveStartCommits stores the start commits of a run.
func SaveStartCommits(ctx context.Context, st Store, runID string, list []RepoStart) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	return st.KVSet(ctx, StartCommitsKey(runID), string(b))
}

// RunRepoRefsKey is the key/value store key of the starting repository refs of
// one run that has no work item (a judge run).
func RunRepoRefsKey(runID string) string { return "engine:run:" + runID + ":repo_refs" }

// ---------- Definition versions ----------

// The states of a definition version.
const (
	DefinitionActive     = "active"
	DefinitionSuperseded = "superseded"
	DefinitionInvalid    = "invalid"
	DefinitionProposed   = "proposed"
	DefinitionApproved   = "approved"
	DefinitionRejected   = "rejected"
)

// EffectiveState returns the state of v. activeID is the active version of
// the factory.
func (v *DefinitionVersion) EffectiveState(activeID string) string {
	switch {
	case v.State != "":
		return v.State
	case !v.Valid:
		return DefinitionInvalid
	case v.ID == activeID:
		return DefinitionActive
	}
	return DefinitionSuperseded
}

// ---------- Review requests ----------

// ReviewRequester is implemented by a forge client that can request a review
// of a pull request from users. A forge that does not implement it
// gets no review request; the reviewer still gets the inbox item.
type ReviewRequester interface {
	RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error
}

// LoginReader is implemented by a forge client that can say which account
// its credential belongs to. A client with the personal token of a user
// returns the login of that user.
type LoginReader interface {
	Login(ctx context.Context) (string, error)
}
