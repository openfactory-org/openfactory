package scoring

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/sqlite"
)

// storeEngine is the engine calls that the proposals use, on a real store.
type storeEngine struct {
	core.Engine
	st       core.Store
	resolved []string
}

func (e *storeEngine) ApplyDefinition(ctx context.Context, factoryID string, files map[string]string, author, message, commitSHA string) (*core.DefinitionVersion, error) {
	v := &core.DefinitionVersion{ID: core.NewID("defv"), FactoryID: factoryID, Files: files, Valid: true, Author: author, Message: message, State: core.DefinitionActive, CreatedAt: time.Now().UTC()}
	if err := e.st.CreateDefinitionVersion(ctx, v); err != nil {
		return nil, err
	}
	f, err := e.st.GetFactory(ctx, factoryID)
	if err != nil {
		return nil, err
	}
	f.ActiveVersionID = v.ID
	return v, e.st.UpdateFactory(ctx, f)
}

func (e *storeEngine) ResolveInbox(ctx context.Context, inboxID, resolution, actor string) error {
	e.resolved = append(e.resolved, inboxID+"="+resolution)
	it, err := e.st.GetInboxItem(ctx, inboxID)
	if err != nil {
		return err
	}
	it.Resolved, it.Resolution = true, resolution
	return e.st.UpdateInboxItem(ctx, it)
}

// TestProposalsOnEachStore runs the proposal flow and the storage of a judge
// score on SQLite.
func TestProposalsOnEachStore(t *testing.T) {
	ctx := context.Background()
	opens := map[string]func(t *testing.T) core.Store{
		"sqlite": func(t *testing.T) core.Store {
			s, err := sqlite.OpenStore(ctx, filepath.Join(t.TempDir(), "of.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			return s
		},
	}
	for name, open := range opens {
		t.Run(name, func(t *testing.T) {
			st := open(t)
			must := func(err error) {
				t.Helper()
				if err != nil {
					t.Fatal(err)
				}
			}
			must(st.CreateTeam(ctx, &core.Team{ID: "team_1", Name: "acme"}))
			base := map[string]string{
				"factory.yaml":      "name: web\n",
				"agents/a/agent.md": "---\nname: a\n---\nOld.\n",
			}
			must(st.CreateFactory(ctx, &core.Factory{ID: "fac_1", TeamID: "team_1", Name: "web", Source: core.DefinitionManaged}))
			v1 := &core.DefinitionVersion{ID: "defv_1", FactoryID: "fac_1", Files: base, Valid: true, State: core.DefinitionActive, CreatedAt: time.Now().UTC().Add(-time.Hour)}
			must(st.CreateDefinitionVersion(ctx, v1))
			f, err := st.GetFactory(ctx, "fac_1")
			must(err)
			f.ActiveVersionID = v1.ID
			must(st.UpdateFactory(ctx, f))
			author := &core.Run{ID: "run_si", TeamID: "team_1", FactoryID: "fac_1", State: core.RunRunning, Metadata: map[string]string{MetaSelfImprovement: "true"}, CreatedAt: time.Now().UTC()}
			must(st.CreateRun(ctx, author))

			eng := &storeEngine{st: st}
			svc, err := NewService(Options{
				Store: st, Engine: eng, CheckInterval: -1, Logger: slog.New(slog.DiscardHandler),
				Classify: func(context.Context, string, string, string, []core.ScorerLabel) (string, string, error) {
					return "", "", nil
				},
				Compile: func(map[string]string) (*core.CompiledDefinition, []core.ValidationError) {
					return &core.CompiledDefinition{}, nil
				},
			})
			must(err)

			v, err := svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "Say it new", Files: map[string]string{"agents/a/agent.md": "---\nname: a\n---\nNew.\n"}})
			must(err)
			open, err := svc.ListProposals(ctx, "fac_1", false)
			must(err)
			if len(open) != 1 || open[0].ID != v.ID || open[0].State != core.DefinitionProposed || open[0].BaseVersionID != "defv_1" || open[0].AuthorRunID != "run_si" || open[0].InboxItemID == "" {
				t.Fatalf("open proposals %+v", open)
			}
			got, err := svc.ProposalForInboxItem(ctx, "fac_1", v.InboxItemID)
			if err != nil || got == nil || got.ID != v.ID {
				t.Fatalf("proposal of the item: %+v %v", got, err)
			}
			if other, err := svc.ProposalForInboxItem(ctx, "fac_1", "inbox_other"); err != nil || other != nil {
				t.Fatalf("unknown item: %+v %v", other, err)
			}
			diff, err := svc.DiffProposal(ctx, &open[0])
			if err != nil || !strings.Contains(diff, "-Old.") || !strings.Contains(diff, "+New.") {
				t.Fatalf("diff %q %v", diff, err)
			}
			item, err := st.GetInboxItem(ctx, v.InboxItemID)
			if err != nil || item.Kind != core.InboxReview || !strings.Contains(item.Body, "+New.") {
				t.Fatalf("inbox item %+v %v", item, err)
			}

			// A second proposal on the same base conflicts once the first is applied.
			v2, err := svc.ProposeDefinitionChange(ctx, ProposalRequest{RunID: author.ID, Message: "Other", Files: map[string]string{"agents/a/agent.md": "---\nname: a\n---\nOther.\n"}})
			must(err)
			approved, err := svc.ApproveProposal(ctx, "fac_1", v.ID, "user:u1")
			must(err)
			if approved.State != core.DefinitionApproved || approved.AppliedVersionID == "" || approved.ReviewedBy != "user:u1" || approved.ReviewedAt == nil {
				t.Fatalf("approved %+v", approved)
			}
			if _, err := svc.ApproveProposal(ctx, "fac_1", v2.ID, "user:u1"); err == nil || !strings.Contains(err.Error(), "changed after the proposal") {
				t.Fatalf("want a conflict, got %v", err)
			}
			rej, err := svc.RejectProposal(ctx, "fac_1", v2.ID, "user:u1", "No.")
			must(err)
			if rej.State != core.DefinitionRejected || rej.ReviewComment != "No." {
				t.Fatalf("rejected %+v", rej)
			}
			all, err := svc.ListProposals(ctx, "fac_1", true)
			must(err)
			if len(all) != 2 {
				t.Fatalf("all proposals %d", len(all))
			}
			if open, _ := svc.ListProposals(ctx, "fac_1", false); len(open) != 0 {
				t.Fatalf("open after the decisions: %d", len(open))
			}

			// The usage of the judge is stored with the score.
			must(st.ReplaceFactoryResources(ctx, "fac_1", nil, nil, []core.Scorer{{ID: "sc_1", FactoryID: "fac_1", Slug: "s"}}, nil))
			sc := &core.Score{ID: "score_1", ScorerID: "sc_1", RunID: "run_si", Label: "ok", Passed: true, JudgeRunID: "run_j", JudgeUsage: &core.Usage{InputTokens: 5, CostUSD: 0.5}, CreatedAt: time.Now().UTC()}
			must(st.CreateScore(ctx, sc))
			back, err := st.GetScore(ctx, "sc_1", "run_si")
			if err != nil || back.JudgeRunID != "run_j" || back.JudgeUsage == nil || back.JudgeUsage.CostUSD != 0.5 {
				t.Fatalf("score %+v %v", back, err)
			}
		})
	}
}
