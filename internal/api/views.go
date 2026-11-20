package api

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/openfactory-org/openfactory/internal/core"
)

// FactoryView is the API representation of a factory.
type FactoryView struct {
	UID string `json:"uid"`
	core.Factory
	URL string `json:"url,omitempty"`
}

func (s *Server) factoryView(f *core.Factory) FactoryView {
	return FactoryView{UID: f.ID, Factory: *f, URL: s.FactoryURL(f.ID)}
}

// External run states. They are the core run states in capital letters.
// The state starting is PENDING, and the
// state idle (stored by earlier releases) is SUCCEEDED.
const (
	StateQueued    = "QUEUED"
	StatePending   = "PENDING"
	StateClaimed   = "CLAIMED"
	StateRunning   = "RUNNING"
	StateBlocked   = "BLOCKED"
	StateSucceeded = "SUCCEEDED"
	StateFailed    = "FAILED"
	StateError     = "ERROR"
	StateCancelled = "CANCELLED"
)

// ExternalRunState maps an internal run state to the API state.
func ExternalRunState(st core.RunState) string {
	switch st {
	case core.RunQueued:
		return StateQueued
	case core.RunPending, core.RunStarting:
		return StatePending
	case core.RunClaimed:
		return StateClaimed
	case core.RunRunning:
		return StateRunning
	case core.RunBlocked:
		return StateBlocked
	case core.RunError:
		return StateError
	case core.RunIdle, core.RunSucceeded:
		return StateSucceeded
	case core.RunFailed:
		return StateFailed
	case core.RunCancelled:
		return StateCancelled
	default:
		return strings.ToUpper(string(st))
	}
}

// internalRunStates maps an API or internal state filter value to internal states.
func internalRunStates(v string) ([]core.RunState, bool) {
	switch strings.ToUpper(v) {
	case StateQueued:
		return []core.RunState{core.RunQueued}, true
	case StateBlocked:
		return []core.RunState{core.RunBlocked}, true
	case StateError:
		return []core.RunState{core.RunError}, true
	case StatePending:
		return []core.RunState{core.RunPending, core.RunStarting}, true
	case StateClaimed:
		return []core.RunState{core.RunClaimed}, true
	case "STARTING":
		return []core.RunState{core.RunStarting}, true
	case StateRunning:
		return []core.RunState{core.RunRunning}, true
	case StateSucceeded:
		// The API state SUCCEEDED includes the state idle that earlier releases stored.
		return []core.RunState{core.RunSucceeded, core.RunIdle}, true
	case StateFailed:
		return []core.RunState{core.RunFailed}, true
	case StateCancelled, "CANCELED":
		return []core.RunState{core.RunCancelled}, true
	}
	return nil, false
}

// StatusMessage is the status of a run for people.
type StatusMessage struct {
	Message     string `json:"message"`
	FailureCode string `json:"failure_code,omitempty"`
	Retryable   bool   `json:"retryable"`
	Detail      string `json:"detail,omitempty"`
}

// UsageView is the usage of a run. Coverage is known, partial or unavailable.
type UsageView struct {
	core.Usage
	Coverage string `json:"coverage"`
}

// RunView is the API representation of a run. The fields blocked_reason,
// failure_code, parent_run_id and error come from the embedded core.Run.
type RunView struct {
	RunID  string `json:"run_id"`
	State  string `json:"state"`
	Phase  string `json:"phase"`
	Title  string `json:"title"`
	RunURL string `json:"run_url"`
	// URL is the dashboard URL of the run. It replaces session_link.
	URL string `json:"url"`
	// Source is the closed source of the run, from core.RunSources. It
	// replaces triggered_by.
	Source core.RunSource `json:"source"`
	// SourceRef is the ID of the automation, schedule, webhook, parent run,
	// scorer or benchmark of the run.
	SourceRef string `json:"source_ref,omitempty"`
	// TriggeredBy hides the stored value of the embedded run. It is always
	// empty, so the field is not in the response.
	TriggeredBy string `json:"triggered_by,omitempty"`
	// StatusMessage replaces the old string. The old text is in error.
	StatusMessage *StatusMessage `json:"status_message,omitempty"`
	// SandboxRunning is true when the newest session is assigned or active.
	SandboxRunning  bool      `json:"sandbox_running"`
	RootRunID       string    `json:"root_run_id"`
	Depth           int       `json:"depth"`
	ActiveChildRuns int       `json:"active_child_runs"`
	Session         int       `json:"session"`
	Usage           UsageView `json:"usage"`
	// JudgeUsage is the usage of the judge runs of the scores of this run.
	// It is set on GET /runs/{runId} only, and only when the run has
	// scores. It adds to the cost of the scored run.
	JudgeUsage *core.Usage `json:"judge_usage,omitempty"`
	UpdatedAt  time.Time   `json:"updated_at"`
	core.Run
}

// runView builds the view of a run from the run row alone. It needs no
// store call, so root_run_id, depth, active_child_runs, session and
// sandbox_running have their defaults. runViewCtx fills them.
func (s *Server) runView(r *core.Run) RunView {
	upd := r.CreatedAt
	for _, t := range []*time.Time{r.StartedAt, r.FinishedAt} {
		if t != nil && t.After(upd) {
			upd = *t
		}
	}
	title := r.Name
	if title == "" {
		title = deriveTitle(r.Prompt)
	}
	src, ref := r.SourceAndRef()
	return RunView{
		RunID:         r.ID,
		State:         ExternalRunState(r.State),
		Phase:         string(r.State),
		Title:         title,
		RunURL:        s.RunURL(r.ID),
		URL:           s.RunURL(r.ID),
		Source:        src,
		SourceRef:     ref,
		StatusMessage: statusMessage(r),
		RootRunID:     r.ID,
		Usage:         UsageView{Usage: r.Usage, Coverage: r.Usage.CoverageLabel()},
		UpdatedAt:     upd,
		Run:           *r,
	}
}

// statusMessage builds the status object of a run. It is nil when the run
// has no text, no failure code and no reason to explain.
func statusMessage(r *core.Run) *StatusMessage {
	sm := StatusMessage{Message: r.Error, FailureCode: string(r.FailureCode)}
	var detail []string
	if info, ok := core.LookupFailure(r.FailureCode); ok {
		sm.Retryable = info.Retry
		detail = append(detail, "failure class: "+string(info.Class))
	}
	switch r.State {
	case core.RunBlocked:
		if r.BlockedReason != "" {
			detail = append(detail, "blocked reason: "+r.BlockedReason)
		}
	case core.RunCancelled:
		if r.CancelledBy != "" {
			detail = append(detail, "cancelled by: "+r.CancelledBy)
		}
	}
	sm.Detail = strings.Join(detail, ", ")
	if sm.Message == "" && sm.FailureCode == "" && sm.Detail == "" {
		return nil
	}
	return &sm
}

// WorkItemView is the API representation of a work item.
type WorkItemView struct {
	core.WorkItem
	URL string `json:"url"`
}

func (s *Server) workItemView(w *core.WorkItem) WorkItemView {
	return WorkItemView{WorkItem: *w, URL: s.WorkItemURL(w.FactoryID, w.ID)}
}

// WebhookView is the API representation of a webhook.
type WebhookView struct {
	core.Webhook
	IngressURL string `json:"ingress_url"`
}

func (s *Server) webhookView(w *core.Webhook) WebhookView {
	u := s.WebhookIngressURL(w.ID)
	if w.AuthMode == "url_token" {
		u += "/{token}"
	}
	return WebhookView{Webhook: *w, IngressURL: u}
}

// deriveTitle builds a short title from a prompt's first non-empty line.
func deriveTitle(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#*->"))
		if line == "" {
			continue
		}
		return truncate(line, 80)
	}
	return "Untitled task"
}

// truncate shortens s to at most n runes. When s is longer, it keeps the
// complete sentences that fit if they fill at least half of n; that text
// ends at a sentence end and gets no ellipsis. Else it cuts mid-sentence
// and adds "…".
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	head := r[:n]
	for i := len(head) - 1; i >= n/2; i-- {
		if isSentenceEnd(head[i]) && (i+1 == len(r) || unicode.IsSpace(r[i+1])) {
			return string(head[:i+1])
		}
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func isSentenceEnd(c rune) bool { return c == '.' || c == '!' || c == '?' }
