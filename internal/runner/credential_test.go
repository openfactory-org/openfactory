package runner

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// The runner drops the default model credential when its environment holds
// a model credential for the harness, and keeps it otherwise.
func TestDropDefaultAuth(t *testing.T) {
	// The process environment of the test must hold no credential.
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"CLAUDE_CODE_USE_VERTEX", "OPENAI_API_KEY", "CODEX_API_KEY"} {
		t.Setenv(k, "")
	}
	auth := map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-fake"}
	tests := []struct {
		name      string
		harness   string
		env       map[string]string
		secretEnv map[string]string
		wantKept  bool
	}{
		{name: "no credential in the environment", harness: "claude", wantKept: true},
		{name: "environment variable", harness: "claude", env: map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}},
		{name: "agent secret", harness: "claude", secretEnv: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok-other"}},
		{name: "vertex switch", harness: "claude", env: map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}},
		{name: "credential of another harness", harness: "claude", env: map[string]string{"OPENAI_API_KEY": "sk-fake"}, wantKept: true},
		{name: "codex key", harness: "codex", env: map[string]string{"OPENAI_API_KEY": "sk-fake"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev, err := runnerclient.OpenEventLog(filepath.Join(t.TempDir(), "events.jsonl"), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ev.Close() })
			r := &runner{log: quietLog(), events: ev, flushCh: make(chan struct{}, 1), harnessAuth: auth, secretEnv: tc.secretEnv,
				start: runnerproto.StartResponse{Env: tc.env, Harness: runnerproto.Harness{Type: tc.harness}}}
			r.dropDefaultAuth()
			if kept := r.harnessAuth != nil; kept != tc.wantKept {
				t.Errorf("kept = %v, want %v", kept, tc.wantKept)
			}
			evs, err := r.events.After(0, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			diag := len(evs) == 1 && evs[0].Type == runnerproto.EventDiagnostic && strings.Contains(string(evs[0].Data), defaultCredentialNotUsed)
			if diag == tc.wantKept {
				t.Errorf("diagnostic = %v with kept = %v: %+v", diag, tc.wantKept, evs)
			}
		})
	}
}
