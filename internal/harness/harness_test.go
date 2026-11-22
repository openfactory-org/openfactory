package harness_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/harness/harnesstest"
	"github.com/openfactory-org/openfactory/internal/rtcore"
)

type fakeHarness struct{ t rtcore.HarnessType }

func (f fakeHarness) Type() rtcore.HarnessType { return f.t }
func (f fakeHarness) Run(context.Context, rtcore.Sandbox, rtcore.HarnessRequest) (<-chan rtcore.HarnessEvent, error) {
	return nil, nil
}

func TestRegistry(t *testing.T) {
	r := harness.NewRegistry(fakeHarness{rtcore.HarnessClaude}, fakeHarness{rtcore.HarnessCodex})
	for _, in := range []rtcore.HarnessType{"claude", "claude-code", "Claude-Code"} {
		h, err := r.Get(in)
		if err != nil || h.Type() != rtcore.HarnessClaude {
			t.Fatalf("Get(%q) = %v, %v", in, h, err)
		}
	}
	if h, err := r.Get(""); err != nil || h.Type() != rtcore.HarnessClaude {
		t.Fatalf("empty should select claude: %v", err)
	}
	if _, err := r.Get("open"); !errors.Is(err, rtcore.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if got := r.Types(); len(got) != 2 || got[0] != rtcore.HarnessClaude {
		t.Fatalf("types = %v", got)
	}
	if harness.NormalizeType("claude-code") != rtcore.HarnessClaude || !harness.IsKnownType("codex") || harness.IsKnownType("gemini") || harness.IsKnownType("open") {
		t.Fatal("normalize")
	}
}

func TestClaudeAuthEnv(t *testing.T) {
	tests := []struct {
		name    string
		auth    map[string]string
		want    map[string]string
		wantErr bool
	}{
		{"empty", nil, map[string]string{}, false},
		{"api key env", map[string]string{"ANTHROPIC_API_KEY": "k"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, false},
		{"typed api key", map[string]string{"type": "anthropic_api_key", "value": "k"}, map[string]string{"ANTHROPIC_API_KEY": "k"}, false},
		{"typed oauth token", map[string]string{"type": "anthropic_oauth_token", "value": " tok "}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}, false},
		{"oauth token env", map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok"}, false},
		{"empty oauth token", map[string]string{"type": "anthropic_oauth_token", "value": ""}, nil, true},
		{"unknown type rejected", map[string]string{"type": "generic", "value": "b"}, nil, true},
		{"openai rejected", map[string]string{"type": "openai_api_key", "value": "sk"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := harness.ClaudeAuthEnv(tt.auth)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v", err)
			}
			if tt.wantErr {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Fatalf("got %v want %v", got, tt.want)
				}
			}
		})
	}
}

func TestCodexAuthEnv(t *testing.T) {
	got, err := harness.CodexAuthEnv(map[string]string{"OPENAI_API_KEY": "sk"})
	if err != nil || got["OPENAI_API_KEY"] != "sk" || got["CODEX_API_KEY"] != "sk" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := harness.CodexAuthEnv(map[string]string{"type": "anthropic_api_key", "value": "k"}); !errors.Is(err, harness.ErrAuthMismatch) {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillsIndexAndDedupe(t *testing.T) {
	skills := []rtcore.Skill{
		{Name: "review", Description: "factory", Scope: "factory", Content: "F"},
		{Name: "Review", Description: "agent", Scope: "implement", Content: "A"},
		{Name: "triage", Description: "t", Scope: "factory"},
	}
	d := harness.DedupeSkills(skills)
	if len(d) != 2 || d[0].Content != "A" {
		t.Fatalf("dedupe = %+v", d)
	}
	idx := harness.SkillsIndex(skills, ".openfactory/skills")
	if !strings.Contains(idx, "`.openfactory/skills/review/SKILL.md`") || strings.Contains(idx, "factory") && strings.Count(idx, "review") > 2 {
		t.Fatalf("index = %s", idx)
	}
	sb := harnesstest.New("/w")
	sb.Files["/w/.claude/skills/review/SKILL.md"] = []byte("repo-owned")
	if _, err := harness.WriteSkills(context.Background(), sb, "/w", skills, harness.SkillWriteOptions{Dirs: []string{".claude/skills"}, SkipExisting: true}); err != nil {
		t.Fatal(err)
	}
	if sb.File("/w/.claude/skills/review/SKILL.md") != "repo-owned" {
		t.Fatal("existing repo skill overwritten")
	}
}

func TestTOMLHelpers(t *testing.T) {
	if harness.TOMLString("a\"b\\c\n") != `"a\"b\\c\n"` {
		t.Fatal(harness.TOMLString("a\"b\\c\n"))
	}
	if harness.TOMLKey("ok-key_1") != "ok-key_1" || harness.TOMLKey("a.b") != `"a.b"` {
		t.Fatal("keys")
	}
	cfg, warn := harness.ClaudeMCPConfig(map[string]rtcore.MCPServer{"s": {URL: "https://x/sse"}, "m": {ManagedID: "id"}})
	if !strings.Contains(string(cfg), `"type": "sse"`) || len(warn) != 1 {
		t.Fatalf("cfg=%s warn=%v", cfg, warn)
	}
}

func TestTruncate(t *testing.T) {
	s := strings.Repeat("a", 100) + strings.Repeat("b", 100)
	out := harness.Truncate(s, 60)
	if !strings.HasPrefix(out, "aaaa") || !strings.HasSuffix(out, "bbbb") || !strings.Contains(out, "truncated") {
		t.Fatal(out)
	}
}

func TestCredentialHint(t *testing.T) {
	run := func(a *rtcore.HarnessAuth) rtcore.HarnessConfig {
		return rtcore.HarnessConfig{Type: rtcore.HarnessClaude, Auth: a}
	}
	if h := harness.CredentialHint(rtcore.HarnessRequest{Harness: run(nil)}); !strings.Contains(h, "harness.auth") {
		t.Errorf("no hint without a credential: %q", h)
	}
	if h := harness.CredentialHint(rtcore.HarnessRequest{Harness: run(nil), Auth: map[string]string{"ANTHROPIC_API_KEY": "k"}}); h != "" {
		t.Errorf("hint with auth: %q", h)
	}
	if h := harness.CredentialHint(rtcore.HarnessRequest{Harness: run(&rtcore.HarnessAuth{Source: "workerEnvironment"})}); h != "" {
		t.Errorf("hint with worker environment auth: %q", h)
	}
}
