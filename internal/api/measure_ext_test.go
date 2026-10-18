package api

import (
	"context"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

type fakeProposals struct {
	list     []core.DefinitionVersion
	approved []string
	rejected []string
}

func (f *fakeProposals) ListProposals(_ context.Context, factoryID string, all bool) ([]core.DefinitionVersion, error) {
	return f.list, nil
}

func (f *fakeProposals) ProposalForInboxItem(_ context.Context, factoryID, inboxID string) (*core.DefinitionVersion, error) {
	for i := range f.list {
		if f.list[i].InboxItemID == inboxID && f.list[i].State == core.DefinitionProposed {
			return &f.list[i], nil
		}
	}
	return nil, nil
}

func (f *fakeProposals) DiffProposal(_ context.Context, v *core.DefinitionVersion) (string, error) {
	return "--- a/x\n+++ b/x\n-old\n+new\n", nil
}

func (f *fakeProposals) ApproveProposal(_ context.Context, factoryID, id, actor string) (*core.DefinitionVersion, error) {
	if id == "defv_conflict" {
		return nil, core.ErrConflict
	}
	if id != "defv_p" {
		return nil, core.ErrNotFound
	}
	f.approved = append(f.approved, id+"/"+actor)
	return &core.DefinitionVersion{ID: id, FactoryID: factoryID, State: core.DefinitionApproved, AppliedVersionID: "defv_new"}, nil
}

func (f *fakeProposals) RejectProposal(_ context.Context, factoryID, id, actor, comment string) (*core.DefinitionVersion, error) {
	f.rejected = append(f.rejected, id+"/"+comment)
	return &core.DefinitionVersion{ID: id, FactoryID: factoryID, State: core.DefinitionRejected, ReviewComment: comment}, nil
}

func TestBenchmarkCancelAndTaskEndpoints(t *testing.T) {
	var cancelled []string
	var taskReq BenchmarkTaskRequest
	e := newTestEnv(t, func(o *Options) {
		o.CancelBenchmark = func(_ context.Context, id, actor string) (*core.BenchmarkRun, error) {
			cancelled = append(cancelled, id+"/"+actor)
			return &core.BenchmarkRun{ID: id, State: "cancelled"}, nil
		}
		o.BenchmarkTaskFromRun = func(_ context.Context, req BenchmarkTaskRequest) (any, map[string]string, map[string]string, error) {
			taskReq = req
			return map[string]any{"slug": "t1"}, map[string]string{"benchmarks/s/tasks/t1.yaml": "title: t"}, nil, nil
		}
	})
	ctx := context.Background()
	fid := e.createFactory("bench")
	_ = e.store.CreateBenchmarkRun(ctx, &core.BenchmarkRun{ID: "bench_1", FactoryID: fid, Suite: "s", State: "running", CreatedAt: time.Now().UTC()})
	e.must(e.do("POST", "/benchmark-runs/bench_1/cancel", e.viewer.Key, nil), 403)
	e.must(e.do("POST", "/benchmark-runs/bench_1/cancel", e.other.Key, nil), 404)
	r := e.must(e.do("POST", "/benchmark-runs/bench_1/cancel", e.editor.Key, nil), 200)
	if r.Body["state"] != "cancelled" || len(cancelled) != 1 {
		t.Fatalf("cancel: %v %v", r.Body, cancelled)
	}
	e.must(e.do("POST", "/benchmark-runs/nope/cancel", e.editor.Key, nil), 404)

	e.must(e.do("POST", "/factories/"+fid+"/benchmarks/s/tasks", e.editor.Key, map[string]any{}), 400)
	e.must(e.do("POST", "/factories/"+fid+"/benchmarks/s/tasks", e.editor.Key, map[string]any{"from_run_id": "missing", "success_criteria": "c"}), 404)
	run := &core.Run{ID: "run_src", TeamID: e.team.ID, FactoryID: fid, State: core.RunSucceeded, Prompt: "p", CreatedAt: time.Now().UTC()}
	if err := e.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	// A git definition gets the files back and nothing is applied.
	f, _ := e.store.GetFactory(ctx, fid)
	f.Source = core.DefinitionGit
	_ = e.store.UpdateFactory(ctx, f)
	r = e.must(e.do("POST", "/factories/"+fid+"/benchmarks/s/tasks", e.editor.Key, map[string]any{"from_run_id": "run_src", "success_criteria": "c"}), 201)
	if r.Body["applied"] != false || taskReq.Suite != "s" || taskReq.FromRunID != "run_src" || taskReq.FactoryID != fid {
		t.Fatalf("task: %v %+v", r.Body, taskReq)
	}
}

func TestProposalEndpoints(t *testing.T) {
	fp := &fakeProposals{}
	e := newTestEnv(t, func(o *Options) { o.Proposals = fp })
	fid := e.createFactory("prop")
	fp.list = []core.DefinitionVersion{{ID: "defv_p", FactoryID: fid, State: core.DefinitionProposed, Message: "m", AuthorRunID: "run_1", CreatedAt: time.Now().UTC()}}
	l := e.must(e.do("GET", "/factories/"+fid+"/definition/proposals", e.viewer.Key, nil), 200)
	items := list(l.Body, "proposals")
	if len(items) != 1 || items[0].(map[string]any)["diff"] == "" || items[0].(map[string]any)["state"] != "proposed" {
		t.Fatalf("list: %v", l.Body)
	}
	e.must(e.do("POST", "/factories/"+fid+"/definition/proposals/defv_p/approve", e.viewer.Key, nil), 403)
	a := e.must(e.do("POST", "/factories/"+fid+"/definition/proposals/defv_p/approve", e.editor.Key, nil), 200)
	if a.Body["applied_version_id"] != "defv_new" || len(fp.approved) != 1 {
		t.Fatalf("approve: %v", a.Body)
	}
	e.must(e.do("POST", "/factories/"+fid+"/definition/proposals/defv_conflict/approve", e.editor.Key, nil), 409)
	e.must(e.do("POST", "/factories/"+fid+"/definition/proposals/nope/approve", e.editor.Key, nil), 404)
	rj := e.must(e.do("POST", "/factories/"+fid+"/definition/proposals/defv_p/reject", e.editor.Key, map[string]any{"comment": "no"}), 200)
	if rj.Body["state"] != "rejected" || fp.rejected[0] != "defv_p/no" {
		t.Fatalf("reject: %v %v", rj.Body, fp.rejected)
	}
}

// TestProposalReviewItemResolve shows that one click on the review item of a
// proposal applies it or rejects it.
func TestProposalReviewItemResolve(t *testing.T) {
	fp := &fakeProposals{}
	e := newTestEnv(t, func(o *Options) { o.Proposals = fp })
	ctx := context.Background()
	fid := e.createFactory("review")
	fp.list = []core.DefinitionVersion{{ID: "defv_p", FactoryID: fid, State: core.DefinitionProposed, Message: "m", InboxItemID: "inb_p", CreatedAt: time.Now().UTC()}}
	for _, id := range []string{"inb_p", "inb_other"} {
		if err := e.store.CreateInboxItem(ctx, &core.InboxItem{ID: id, FactoryID: fid, Kind: core.InboxReview, Title: "Review " + id, Options: []string{"approve", "reject"}}); err != nil {
			t.Fatal(err)
		}
	}
	// A user without the editor role cannot apply the change.
	e.must(e.do("POST", "/inbox/inb_p/resolve", e.viewer.Key, map[string]any{"resolution": "approve"}), 403)
	// Only approve and reject are valid for a proposal.
	e.must(e.do("POST", "/inbox/inb_p/resolve", e.editor.Key, map[string]any{"resolution": "maybe"}), 400)
	e.must(e.do("POST", "/inbox/inb_p/resolve", e.editor.Key, map[string]any{"resolution": "approve"}), 200)
	if len(fp.approved) != 1 || fp.approved[0] != "defv_p/"+"user:"+e.editor.User.ID {
		t.Fatalf("approved %v", fp.approved)
	}
	fp.list[0].State = core.DefinitionProposed
	e.must(e.do("POST", "/inbox/inb_p/resolve", e.editor.Key, map[string]any{"resolution": "Reject: too broad"}), 200)
	if len(fp.rejected) != 1 || fp.rejected[0] != "defv_p/too broad" {
		t.Fatalf("rejected %v", fp.rejected)
	}
	// A review item without a proposal (a git definition) resolves as before.
	e.must(e.do("POST", "/inbox/inb_other/resolve", e.viewer.Key, map[string]any{"resolution": "resolved"}), 200)
}

// TestRunShowsJudgeUsage shows that the usage of the judge runs adds to the
// cost of the scored run: GET /runs/{id} has judge_usage.
func TestRunShowsJudgeUsage(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("judged")
	run := &core.Run{ID: "run_scored", TeamID: e.team.ID, FactoryID: fid, State: core.RunSucceeded, Prompt: "p", CreatedAt: time.Now().UTC()}
	if err := e.store.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	r := e.must(e.do("GET", "/runs/run_scored", e.viewer.Key, nil), 200)
	if _, ok := r.Body["judge_usage"]; ok {
		t.Fatalf("a run without scores has judge_usage: %v", r.Body)
	}
	if err := e.store.ReplaceFactoryResources(ctx, fid, nil, nil, []core.Scorer{{ID: "sc_a", FactoryID: fid, Slug: "a"}, {ID: "sc_b", FactoryID: fid, Slug: "b"}}, nil); err != nil {
		t.Fatal(err)
	}
	for i, sc := range []string{"sc_a", "sc_b"} {
		u := &core.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.25 * float64(i+1)}
		if err := e.store.CreateScore(ctx, &core.Score{ID: "s_" + sc, ScorerID: sc, RunID: run.ID, JudgeRunID: "judge_" + sc, JudgeUsage: u, CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
	}
	r = e.must(e.do("GET", "/runs/run_scored", e.viewer.Key, nil), 200)
	ju, _ := r.Body["judge_usage"].(map[string]any)
	if ju == nil || ju["cost_usd"] != 0.75 || ju["input_tokens"] != float64(200) {
		t.Fatalf("judge_usage = %v", r.Body["judge_usage"])
	}
}

// TestDefinitionVersionStates shows the state of each version in the API:
// active, superseded or proposed.
func TestDefinitionVersionStates(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fid := e.createFactory("states")
	f, err := e.store.GetFactory(ctx, fid)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, v := range []core.DefinitionVersion{
		{ID: "defv_old", Valid: true, CreatedAt: now.Add(-3 * time.Hour)},
		{ID: "defv_cur", Valid: true, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: "defv_prop", Valid: true, State: core.DefinitionProposed, BaseVersionID: "defv_cur", CreatedAt: now.Add(-time.Hour)},
		{ID: "defv_bad", Valid: false, CreatedAt: now.Add(-30 * time.Minute)},
	} {
		v.FactoryID = fid
		if err := e.store.CreateDefinitionVersion(ctx, &v); err != nil {
			t.Fatalf("version %d: %v", i, err)
		}
	}
	f.ActiveVersionID = "defv_cur"
	if err := e.store.UpdateFactory(ctx, f); err != nil {
		t.Fatal(err)
	}
	r := e.must(e.do("GET", "/factories/"+fid+"/definition/versions", e.viewer.Key, nil), 200)
	got := map[string]string{}
	for _, it := range list(r.Body, "versions") {
		m := it.(map[string]any)
		got[m["id"].(string)], _ = m["state"].(string)
	}
	if got["defv_old"] != "superseded" || got["defv_cur"] != "active" || got["defv_prop"] != "proposed" || got["defv_bad"] != "invalid" {
		t.Fatalf("states %v", got)
	}
	one := e.must(e.do("GET", "/factories/"+fid+"/definition/versions/defv_cur", e.viewer.Key, nil), 200)
	if one.Body["state"] != "active" {
		t.Fatalf("version: %v", one.Body)
	}
}
