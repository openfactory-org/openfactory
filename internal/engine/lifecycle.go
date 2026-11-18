package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/links"
)

// lifecycleResultMax is the most bytes of a child result that a lifecycle
// message carries.
const lifecycleResultMax = 6144

// lifecycleData is the data of a lifecycle message. The field order is part of
// the message format.
type lifecycleData struct {
	Event           string `json:"event"`
	RunID           string `json:"run_id"`
	Name            string `json:"name"`
	Agent           string `json:"agent"`
	FailureCode     string `json:"failure_code"`
	Result          string `json:"result"`
	ResultTruncated bool   `json:"result_truncated"`
	URL             string `json:"url"`
}

// lifecycleEvent maps a run state to the event name that the parent gets.
func lifecycleEvent(st core.RunState) string {
	switch st {
	case core.RunSucceeded:
		return "succeeded"
	case core.RunBlocked:
		return "blocked"
	case core.RunFailed:
		return "failed"
	case core.RunError:
		return "error"
	case core.RunCancelled:
		return "cancelled"
	}
	return ""
}

// truncateUTF8 cuts s to at most max bytes on a UTF-8 boundary.
func truncateUTF8(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}

// runURL is the dashboard URL of a run, or empty without a public URL.
func (e *Engine) runURL(runID string) string {
	if e.publicURL == "" {
		return ""
	}
	return links.Run(e.publicURL, runID)
}

// lifecycleTarget returns the run that gets the lifecycle messages of r, and
// whether r is a side run. For a child run it is the parent. For a side run
// of a work item it is the root run of the work item, and only while
// the work item is active. It returns "" for each other run, and for the root
// run of a work item.
func (e *Engine) lifecycleTarget(ctx context.Context, r *core.Run) (to string, side bool) {
	if r.ParentRunID != "" {
		return r.ParentRunID, false
	}
	if r.WorkItemID == "" {
		return "", false
	}
	wi, err := e.store.GetWorkItem(ctx, r.WorkItemID)
	if err != nil || wi.Stage.Terminal() || wi.RootRunID == "" || wi.RootRunID == r.ID {
		return "", false
	}
	return wi.RootRunID, true
}

// lifecycleParts makes the data and the text of a lifecycle message about the
// child run, or side run, child. Both paths use it.
func (e *Engine) lifecycleParts(child *core.Run, side bool, event string) (data []byte, content string, err error) {
	result, truncated := truncateUTF8(child.Result, lifecycleResultMax)
	data, err = json.Marshal(lifecycleData{
		Event:           event,
		RunID:           child.ID,
		Name:            child.Name,
		Agent:           child.AgentName,
		FailureCode:     string(child.FailureCode),
		Result:          result,
		ResultTruncated: truncated,
		URL:             e.runURL(child.ID),
	})
	if err != nil {
		return nil, "", err
	}
	label := child.Name
	if label == "" {
		label = child.ID
	}
	kind := "Child run "
	if side {
		kind = "Side run "
	}
	var b strings.Builder
	b.WriteString(kind + strconv.Quote(label) + " " + event + ".")
	if (event == "failed" || event == "error") && child.FailureCode != "" {
		b.WriteString(" Failure code: " + string(child.FailureCode) + ".")
	}
	return data, b.String(), nil
}

// sendLifecycle tells the run to that the child run, or side run, started,
// finished a turn or was cancelled (see lifecycleTarget).
func (e *Engine) sendLifecycle(ctx context.Context, child *core.Run, to string, side bool, event string) error {
	if to == "" {
		return nil
	}
	data, content, err := e.lifecycleParts(child, side, event)
	if err != nil {
		return fmt.Errorf("%w: encode lifecycle message of %s: %w", core.ErrInvalid, child.ID, err)
	}
	dedupe := "lifecycle:" + child.ID + ":" + event + ":" + strconv.FormatInt(child.Turns, 10)
	if event == "started" {
		dedupe = "lifecycle:" + child.ID + ":started"
	}
	in := &core.RunInput{
		ID:        core.NewID("msg"),
		RunID:     to,
		Kind:      "lifecycle",
		Sender:    core.RunInputSender{Type: "run", ID: child.ID, Name: child.Name},
		Content:   content,
		Data:      data,
		DedupeKey: dedupe,
	}
	if _, err := e.runner.Send(ctx, in); err != nil {
		return fmt.Errorf("send lifecycle message of %s to %s (%s): %w", child.ID, to, event, err)
	}
	return nil
}
