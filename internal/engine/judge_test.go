package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

func TestStartJudgeRun(t *testing.T) {
	env := NewTestEnv(t)
	ctx := context.Background()
	refs := []core.RepoAtCommit{{CodeForge: "GITHUB", Owner: "acme", Repo: "payments", Ref: strings.Repeat("a", 40)}}
	req := core.JudgeRunRequest{
		FactoryID:        env.Factory.ID,
		Name:             "scorer-tests-run",
		Instructions:     "Judge the run.",
		Prompt:           "Score run_x.",
		Harness:          core.HarnessConfig{Type: core.HarnessClaude, Model: "m", Auth: &core.HarnessAuth{SecretName: "K"}},
		Machine:          "big",
		Secrets:          []string{"JUDGE"},
		MCPServers:       map[string]core.MCPServer{"docs": {URL: "https://docs.example.com/mcp"}},
		Files:            map[string]string{"transcript.md": "the full transcript"},
		StartingRepoRefs: refs,
		Metadata:         map[string]string{"judge": "score"},
		TriggeredBy:      "scorer:sc_1",
	}
	run, err := env.Engine.StartJudgeRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := env.Store.GetRun(ctx, run.ID)
	cfg := got.Config
	if got.WorkItemID != "" || got.FactoryID != env.Factory.ID || got.TriggeredBy != "scorer:sc_1" || got.State != core.RunQueued {
		t.Fatalf("judge run %+v", got)
	}
	if cfg.Harness.Model != "m" || cfg.Harness.Auth == nil || cfg.Machine == nil || cfg.Machine.VCPUs != 8 || !cfg.IdleTimeoutSet || cfg.IdleTimeoutMinutes != 0 {
		t.Fatalf("config %+v", cfg)
	}
	hasSecret := false
	for _, s := range cfg.Secrets {
		hasSecret = hasSecret || s == "JUDGE"
	}
	if !hasSecret || cfg.MCPServers["docs"].URL == "" || cfg.Instructions != "Judge the run." {
		t.Fatalf("secrets %v mcp %v", cfg.Secrets, cfg.MCPServers)
	}
	if len(cfg.Skills) != 1 || cfg.Skills[0].Name != core.JudgeSkillName || cfg.Skills[0].Files["transcript.md"] != "the full transcript" {
		t.Fatalf("skills %+v", cfg.Skills)
	}
	loaded, err := loadRunRepoRefs(ctx, env.Store, got)
	if err != nil || len(loaded) != 1 || loaded[0].Ref != refs[0].Ref {
		t.Fatalf("repo refs of the judge run: %+v %v", loaded, err)
	}

	bad := req
	bad.Harness = core.HarnessConfig{}
	if _, err := env.Engine.StartJudgeRun(ctx, bad); err == nil {
		t.Fatal("want an error without a harness")
	}
	bad = req
	bad.Machine = "ghost"
	if _, err := env.Engine.StartJudgeRun(ctx, bad); err == nil {
		t.Fatal("want an error for an unknown machine")
	}
}

func TestRecordStartCommits(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	sha := strings.Repeat("b", 40)
	recordStartCommits(ctx, st, "run_1", []core.RepoCheckout{
		{Forge: "GITHUB", Owner: "acme", Name: "api", Path: "api", Ref: sha},
		{Forge: "GITHUB", Owner: "acme", Name: "web", Path: "web"},
	})
	got, err := core.LoadStartCommits(ctx, st, "run_1")
	if err != nil || len(got) != 2 || !got[0].Resolved || got[0].Ref != sha || got[1].Resolved {
		t.Fatalf("start commits %+v %v", got, err)
	}
	// A later session keeps the first record.
	recordStartCommits(ctx, st, "run_1", []core.RepoCheckout{{Forge: "GITHUB", Owner: "acme", Name: "other", Path: "other"}})
	if again, _ := core.LoadStartCommits(ctx, st, "run_1"); len(again) != 2 {
		t.Fatalf("record changed: %+v", again)
	}
}
