package azuredevops

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The plugin serves these two methods as the work item write-back
// (pkg/plugin/sdk.WorkItemTracker).
var _ interface {
	SetWorkItemState(ctx context.Context, project string, id int, state string) error
	AttachPullRequestToWorkItem(ctx context.Context, project string, id int, pr core.PullRequestRef) error
} = (*Client)(nil)

var agileStates = []typeState{
	{"New", "Proposed"}, {"Active", "InProgress"}, {"In Review", "InProgress"}, {"Resolved", "Resolved"}, {"Closed", "Completed"}, {"Removed", "Removed"},
}

func TestPickState(t *testing.T) {
	basic := []typeState{{"To Do", "Proposed"}, {"Doing", "InProgress"}, {"Done", "Completed"}}
	cases := []struct {
		name    string
		state   string
		states  []typeState
		current string
		want    string
	}{
		{"start", core.IssueStateInProgress, agileStates, "New", "Active"},
		{"already active", core.IssueStateInProgress, agileStates, "Active", ""},
		{"never go back from review", core.IssueStateInProgress, agileStates, "In Review", ""},
		{"review state", core.IssueStateInReview, agileStates, "Active", "In Review"},
		{"review from new", core.IssueStateInReview, agileStates, "New", "In Review"},
		{"no review state keeps in progress", core.IssueStateInReview, basic, "To Do", "Doing"},
		{"no review state leaves it", core.IssueStateInReview, basic, "Doing", ""},
		{"review of a closed item", core.IssueStateInReview, agileStates, "Closed", ""},
		{"done", core.IssueStateDone, agileStates, "In Review", "Closed"},
		{"done twice", core.IssueStateDone, agileStates, "Closed", ""},
		{"done after removal", core.IssueStateDone, agileStates, "Removed", ""},
		{"done in the basic process", core.IssueStateDone, basic, "Doing", "Done"},
	}
	for _, tc := range cases {
		if got := pickState(tc.state, tc.states, tc.current); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

type patchCall struct {
	contentType string
	ops         []map[string]any
}

func trackerServer(t *testing.T, current string, relations string, patches *[]patchCall) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/contoso/proj/_apis/wit/workitems/7":
			fmt.Fprintf(w, `{"id":7,"fields":{"System.WorkItemType":"User Story","System.State":%q},"relations":%s}`, current, relations)
		case r.Method == http.MethodGet && r.URL.Path == "/contoso/proj/_apis/wit/workitemtypes/User Story/states":
			b, _ := json.Marshal(map[string]any{"value": agileStates})
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && r.URL.Path == "/contoso/proj/_apis/git/repositories/web":
			fmt.Fprint(w, `{"id":"repo-1","project":{"id":"proj-1"}}`)
		case r.Method == http.MethodPatch && r.URL.Path == "/contoso/proj/_apis/wit/workitems/7":
			b, _ := io.ReadAll(r.Body)
			var ops []map[string]any
			_ = json.Unmarshal(b, &ops)
			*patches = append(*patches, patchCall{contentType: r.Header.Get("Content-Type"), ops: ops})
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{Organization: "contoso", BaseURL: srv.URL, Token: "pat"})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSetWorkItemState(t *testing.T) {
	var patches []patchCall
	c := trackerServer(t, "In Review", `null`, &patches)
	ctx := context.Background()
	if err := c.SetWorkItemState(ctx, "proj", 7, core.IssueStateDone); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 || patches[0].contentType != "application/json-patch+json" {
		t.Fatalf("patches = %+v", patches)
	}
	op := patches[0].ops[0]
	if op["op"] != "add" || op["path"] != "/fields/System.State" || op["value"] != "Closed" {
		t.Fatalf("op = %v", op)
	}
	// In progress does not move a work item that is in review.
	if err := c.SetWorkItemState(ctx, "proj", 7, core.IssueStateInProgress); err != nil || len(patches) != 1 {
		t.Fatalf("in progress patched: %v %+v", err, patches)
	}
	if err := c.SetWorkItemState(ctx, "proj", 7, "later"); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func TestAttachPullRequestToWorkItem(t *testing.T) {
	var patches []patchCall
	c := trackerServer(t, "Active", `null`, &patches)
	ctx := context.Background()
	pr := core.PullRequestRef{Forge: "AZURE_DEVOPS", Repo: "proj/web", Number: 11}
	if err := c.AttachPullRequestToWorkItem(ctx, "proj", 7, pr); err != nil {
		t.Fatal(err)
	}
	if len(patches) != 1 {
		t.Fatalf("patches = %+v", patches)
	}
	op := patches[0].ops[0]
	val, _ := op["value"].(map[string]any)
	if op["path"] != "/relations/-" || val["rel"] != "ArtifactLink" || val["url"] != "vstfs:///Git/PullRequestId/proj-1%2Frepo-1%2F11" {
		t.Fatalf("op = %v", op)
	}

	// The link exists: no second patch.
	patches = nil
	c = trackerServer(t, "Active", `[{"rel":"ArtifactLink","url":"vstfs:///Git/PullRequestId/proj-1%2Frepo-1%2F11"}]`, &patches)
	if err := c.AttachPullRequestToWorkItem(ctx, "proj", 7, pr); err != nil || len(patches) != 0 {
		t.Fatalf("second link: %v %+v", err, patches)
	}
	if err := c.AttachPullRequestToWorkItem(ctx, "proj", 7, core.PullRequestRef{Repo: "proj/web"}); err == nil || !strings.Contains(err.Error(), "number") {
		t.Fatalf("no number: %v", err)
	}
}
