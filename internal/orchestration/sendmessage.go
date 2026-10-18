package orchestration

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/engine"
)

// The rate limit of send_message and follow_up_agent for one sender and one
// target. It stops a loop of messages between two agents.
const (
	sendMessageLimit  = 10
	sendMessageWindow = time.Minute
)

// pairLimiter counts the messages of each sender→target pair in a sliding
// window. The count comes from the messages stored on the target run, so it
// holds across the replicas of the server. A delivery that failed stored no
// message and does not count.
type pairLimiter struct {
	store  core.Store
	limit  int
	window time.Duration
	now    func() time.Time
}

func newPairLimiter(store core.Store, limit int, window time.Duration) *pairLimiter {
	return &pairLimiter{store: store, limit: limit, window: window, now: time.Now}
}

// allow tells if sender can send one more message to target: the run
// target has fewer than limit messages from run sender in the window.
func (l *pairLimiter) allow(ctx context.Context, sender, target string) (bool, error) {
	msgs, err := l.store.ListRunMessages(ctx, target, 0)
	if err != nil {
		return false, err
	}
	actor := "run:" + sender
	cut := l.now().Add(-l.window)
	n := 0
	for _, m := range msgs {
		if m.Role == "user" && m.CreatedAt.After(cut) && engine.MessageActor(m.Data) == actor {
			n++
		}
	}
	return n < l.limit, nil
}

// checkLimit returns a tool error when sender used up its limit of messages
// to target, and nil when it can send.
func (t *tools) checkLimit(ctx context.Context, sender, target string) *mcp.CallToolResult {
	ok, err := t.s.limiter.allow(ctx, sender, target)
	if err != nil {
		return errResult(err)
	}
	if !ok {
		return errResult(fmt.Errorf("rate limit: at most %d messages to run %q in %s; wait for its answer", sendMessageLimit, target, sendMessageWindow))
	}
	return nil
}

type sendMessageInput struct {
	RunID string `json:"run_id" jsonschema:"ID of the target run: your parent, a child, a sibling or a side run of the same work item. list_agents shows them in active_runs"`
	Text  string `json:"text" jsonschema:"The message. The target gets it as an agent message from your run"`
}

// runTarget is a run that this run can send a message to.
type runTarget struct {
	RunID    string `json:"run_id"`
	Name     string `json:"name,omitempty"`
	Agent    string `json:"agent,omitempty"`
	State    string `json:"state"`
	Relation string `json:"relation"`
}

// activeState tells if a run can get a message from send_message. A run that
// finished its turn (succeeded or blocked) waits for messages, so it is
// active. A failed, error or cancelled run is not.
func activeState(s core.RunState) bool {
	switch s {
	case core.RunFailed, core.RunError, core.RunCancelled:
		return false
	}
	return true
}

// relation tells how target relates to sender: parent, child, sibling or
// work_item (an other run of the same work item, for example a side run). It
// returns "" when the sender cannot send a message to target.
func relation(sender, target *core.Run) string {
	switch {
	case target.ID == sender.ID:
		return ""
	case sender.ParentRunID != "" && target.ID == sender.ParentRunID:
		return "parent"
	case target.ParentRunID != "" && target.ParentRunID == sender.ID:
		return "child"
	case sender.WorkItemID == "" || target.WorkItemID != sender.WorkItemID:
		return ""
	case sender.ParentRunID != "" && target.ParentRunID == sender.ParentRunID:
		return "sibling"
	}
	return "work_item"
}

// openWorkItem tells if the work item of a run is still open. A run with no
// work item is open.
func (t *tools) openWorkItem(ctx context.Context, r *core.Run) bool {
	if r.WorkItemID == "" {
		return true
	}
	wi, err := t.s.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil {
		return false
	}
	return !wi.Stage.Terminal()
}

// activeTargets lists the active runs that sender can send a message to.
func (t *tools) activeTargets(ctx context.Context, sender *core.Run) []runTarget {
	var cands []core.Run
	if sender.WorkItemID != "" {
		if !t.openWorkItem(ctx, sender) {
			return nil
		}
		runs, err := t.s.store.ListRuns(ctx, core.RunFilter{WorkItemID: sender.WorkItemID})
		if err != nil {
			return nil
		}
		cands = runs
	} else {
		if sender.ParentRunID != "" {
			if p, err := t.s.store.GetRun(ctx, sender.ParentRunID); err == nil {
				cands = append(cands, *p)
			}
		}
		if kids, err := t.s.store.ListRuns(ctx, core.RunFilter{ParentRunID: sender.ID}); err == nil {
			cands = append(cands, kids...)
		}
	}
	out := []runTarget{}
	seen := map[string]bool{}
	for i := range cands {
		c := &cands[i]
		rel := relation(sender, c)
		if rel == "" || seen[c.ID] || !activeState(c.State) {
			continue
		}
		seen[c.ID] = true
		out = append(out, runTarget{RunID: c.ID, Name: c.Name, Agent: c.AgentName, State: string(c.State), Relation: rel})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RunID < out[j].RunID })
	return out
}

func (t *tools) sendMessage(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, any, error) {
	sender, err := t.activeRun(ctx)
	if err != nil {
		return errResult(err), nil, nil
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return errResult(errors.New("text is required")), nil, nil
	}
	id := strings.TrimSpace(in.RunID)
	if id == sender.ID {
		return errResult(errors.New("you cannot send a message to your own run")), nil, nil
	}
	target, err := t.s.store.GetRun(ctx, id)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return errResult(fmt.Errorf("run %q not found", id)), nil, nil
		}
		return errResult(err), nil, nil
	}
	rel := relation(sender, target)
	if rel == "" {
		return errResult(fmt.Errorf("run %q is not in your work item; you can send messages only to your parent, your children and the other runs of your work item", id)), nil, nil
	}
	if !activeState(target.State) || !t.openWorkItem(ctx, target) {
		return errResult(fmt.Errorf("run %q is not active (state %s)", id, target.State)), nil, nil
	}
	if res := t.checkLimit(ctx, sender.ID, target.ID); res != nil {
		return res, nil, nil
	}
	text = t.s.redactFor(ctx, t.runID)(text)
	if err := t.s.engine.FollowUp(ctx, target.ID, text, t.actor()); err != nil {
		return errResult(err), nil, nil
	}
	return textResult("Message sent to %s (%s).", target.ID, rel), nil, nil
}
