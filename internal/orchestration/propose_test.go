package orchestration

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

// TestProposeDefinitionChangeTool shows the propose tool: it is absent without
// a proposer, it passes the files, the deletions and the message of the run,
// and it returns the errors of the check so the agent can fix the files.
func TestProposeDefinitionChangeTool(t *testing.T) {
	_, _, ts := newFixture(t)
	res, err := connect(t, ts, "run_root").ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == "propose_definition_change" {
			t.Fatal("the tool exists without a proposer")
		}
	}

	st := &fakeStore{runs: map[string]*core.Run{
		"run_si":   {ID: "run_si", FactoryID: "fac", WorkItemID: "wi_1", State: core.RunRunning},
		"run_dead": {ID: "run_dead", FactoryID: "fac", State: core.RunCancelled},
	}}
	type call struct {
		runID   string
		files   map[string]string
		del     []string
		message string
	}
	var calls []call
	srv := newServer(&fakeEngine{}, st)
	srv.SetProposer(func(_ context.Context, runID string, files map[string]string, del []string, message string) (*core.DefinitionVersion, error) {
		calls = append(calls, call{runID, files, del, message})
		if strings.Contains(files["factory.yaml"], "BROKEN") {
			return nil, errors.New("the proposed definition has 1 error(s):\n- factory.yaml: broken on purpose")
		}
		return &core.DefinitionVersion{ID: "defv_p", BaseVersionID: "defv_0"}, nil
	})
	mux := http.NewServeMux()
	srv.Register(mux)
	ts2 := httptest.NewServer(mux)
	t.Cleanup(ts2.Close)
	sess := connect(t, ts2, "run_si")

	text, isErr := callText(t, sess, "propose_definition_change", map[string]any{
		"files":   map[string]any{"agents/implement/agent.md": "new"},
		"delete":  []any{"agents/old/agent.md"},
		"message": "Run the tests",
	})
	if isErr || !strings.Contains(text, "defv_p") || !strings.Contains(text, "defv_0") {
		t.Fatalf("result %q (error %v)", text, isErr)
	}
	if len(calls) != 1 || calls[0].runID != "run_si" || calls[0].files["agents/implement/agent.md"] != "new" || len(calls[0].del) != 1 || calls[0].message != "Run the tests" {
		t.Fatalf("calls %+v", calls)
	}
	text, isErr = callText(t, sess, "propose_definition_change", map[string]any{"files": map[string]any{"factory.yaml": "BROKEN"}, "message": "m"})
	if !isErr || !strings.Contains(text, "broken on purpose") {
		t.Fatalf("errors do not come back: %q (error %v)", text, isErr)
	}
	// A cancelled run cannot propose.
	text, isErr = callText(t, connect(t, ts2, "run_dead"), "propose_definition_change", map[string]any{"files": map[string]any{"a": "b"}, "message": "m"})
	if !isErr || !strings.Contains(text, "cancelled") || len(calls) != 2 {
		t.Fatalf("cancelled run: %q (error %v, calls %d)", text, isErr, len(calls))
	}
}
