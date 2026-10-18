package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestScheduleControls(t *testing.T) {
	e := newTestEnv(t)
	fid := e.createFactory("Payments")
	ctx := context.Background()
	autos := []core.Automation{
		{ID: "aut_nightly", FactoryID: fid, Name: "nightly-report", Enabled: true, Prompt: "Write the report.", Agent: "foreman",
			Triggers: []core.Trigger{{Provider: "schedule", Event: "cron_fired", Concurrency: core.ScheduleConcurrencySkip,
				Schedule: &core.Schedule{Name: "main", Cron: "0 2 * * *"}}}},
		{ID: "aut_push", FactoryID: fid, Name: "on-push", Enabled: false, Prompt: "Check the push.",
			Triggers: []core.Trigger{{Provider: "github", Event: "push"}}},
	}
	agents, _ := e.store.ListAgents(ctx, fid)
	if err := e.store.ReplaceFactoryResources(ctx, fid, agents, autos, nil, nil); err != nil {
		t.Fatal(err)
	}

	l := e.must(e.do("GET", "/factories/"+fid+"/schedules", e.viewer.Key, nil), http.StatusOK)
	items := list(l.Body, "schedules")
	if len(items) != 1 {
		t.Fatalf("schedules: %s", l.Raw)
	}
	s := items[0].(map[string]any)
	if s["id"] != "nightly-report:main" || s["paused"] != false || s["next_run_at"] == nil || s["concurrency"] != "skip" || s["last_run_at"] != nil {
		t.Fatalf("schedule %v", s)
	}

	// A viewer cannot pause; an editor can, without a definition change.
	e.must(e.do("POST", "/factories/"+fid+"/schedules/nightly-report:main/pause", e.viewer.Key, nil), http.StatusForbidden)
	versions := len(list(e.must(e.do("GET", "/factories/"+fid+"/definition/versions", e.viewer.Key, nil), http.StatusOK).Body, "versions"))
	p := e.must(e.do("POST", "/factories/"+fid+"/schedules/nightly-report:main/pause", e.editor.Key, nil), http.StatusOK)
	if p.Body["paused"] != true || p.Body["next_run_at"] != nil || p.Body["paused_by"] == "" {
		t.Fatalf("paused: %s", p.Raw)
	}
	if n := len(list(e.must(e.do("GET", "/factories/"+fid+"/definition/versions", e.viewer.Key, nil), http.StatusOK).Body, "versions")); n != versions {
		t.Fatalf("pause changed the definition versions: %d -> %d", versions, n)
	}
	l = e.must(e.do("GET", "/factories/"+fid+"/schedules", e.viewer.Key, nil), http.StatusOK)
	if list(l.Body, "schedules")[0].(map[string]any)["paused"] != true {
		t.Fatalf("list after pause: %s", l.Raw)
	}
	r := e.must(e.do("POST", "/factories/"+fid+"/schedules/nightly-report:main/resume", e.editor.Key, nil), http.StatusOK)
	if r.Body["paused"] != false || r.Body["next_run_at"] == nil {
		t.Fatalf("resumed: %s", r.Raw)
	}
	e.must(e.do("POST", "/factories/"+fid+"/schedules/nope/pause", e.editor.Key, nil), http.StatusNotFound)
	e.must(e.do("GET", "/factories/"+fid+"/schedules", e.other.Key, nil), http.StatusNotFound)

	// Manual start, also of a disabled automation.
	run := e.must(e.do("POST", "/automations/aut_push/run", e.editor.Key, nil), http.StatusCreated)
	if str(run.Body, "id") == "" {
		t.Fatalf("run: %s", run.Raw)
	}
	var req core.DispatchRequest
	for _, c := range e.engine.Calls {
		if c.Method == "Dispatch" {
			req = c.Args[0].(core.DispatchRequest)
		}
	}
	if req.AutomationID != "aut_push" || req.Prompt != "Check the push." || req.TriggeredBy != "automation:aut_push" || req.Event == nil || req.Event.Type != "manual_run" {
		t.Fatalf("dispatch request %+v", req)
	}
	e.must(e.do("POST", "/automations/aut_push/run", e.viewer.Key, nil), http.StatusForbidden)
	e.must(e.do("POST", "/automations/aut_push/run", e.other.Key, nil), http.StatusNotFound)
	e.must(e.do("POST", "/automations/aut_none/run", e.editor.Key, nil), http.StatusNotFound)
}
