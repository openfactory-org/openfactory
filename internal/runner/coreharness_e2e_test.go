package runner_test

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// scriptedHarness is a rtcore.Harness that runs one command in its sandbox and
// then plays fixed events.
type scriptedHarness struct {
	seen chan rtcore.HarnessRequest
}

func (scriptedHarness) Type() rtcore.HarnessType { return rtcore.HarnessClaude }

func (h scriptedHarness) Run(ctx context.Context, sb rtcore.Sandbox, req rtcore.HarnessRequest) (<-chan rtcore.HarnessEvent, error) {
	h.seen <- req
	// The sandbox runs in the workspace and sees the secret file.
	res, err := sb.Exec(ctx, rtcore.ExecRequest{Cmd: []string{"cat", "secrets/token.txt"}})
	if err != nil {
		return nil, err
	}
	ch := make(chan rtcore.HarnessEvent, 8)
	ch <- rtcore.HarnessEvent{Kind: "message", Role: "assistant", Content: "token file: " + res.Stdout, SessionID: "sess_1"}
	ch <- rtcore.HarnessEvent{Kind: "usage", Usage: &rtcore.Usage{InputTokens: 100, OutputTokens: 20}}
	ch <- rtcore.HarnessEvent{Kind: "done", Content: "all done", SessionID: "sess_1", Usage: &rtcore.Usage{InputTokens: 100, OutputTokens: 20}}
	close(ch)
	return ch, nil
}

func TestCoreHarnessEndToEnd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs cat")
	}
	secrets := &runnerproto.SecretsResponse{
		Files: []runnerproto.SecretFile{{
			Path: "secrets/token.txt", Mode: "0600",
			ContentBase64: base64.StdEncoding.EncodeToString([]byte("s3cret-file")),
		}},
		HarnessAuth: map[string]string{"EXAMPLE_API_KEY": "key-1"},
		MCPHeaders:  map[string]map[string]string{"srv": {"X-Token": "h1"}},
	}
	e := newEnv(t, runnerproto.StartResponse{
		Harness: runnerproto.Harness{
			Type: "claude", Model: "m", SystemPrompt: "sys",
			MCPServers: map[string]runnerproto.MCPServer{"srv": {URL: "https://mcp.example/x"}},
		},
		Workspace: runnerproto.Workspace{
			Skills: []runnerproto.Skill{{Name: "review", Files: map[string]string{"SKILL.md": "be kind"}}},
		},
	}, secrets)
	h := scriptedHarness{seen: make(chan rtcore.HarnessRequest, 1)}
	cfg := e.config(execHarness("cat"))
	cfg.Harnesses = map[string]runner.Harness{"claude": &runner.CoreHarness{H: h}}
	done := e.start(context.Background(), cfg)
	waitFor(t, "ready", e.ready)
	e.startTurn("t1", "hello")
	waitFor(t, "turn.ended", func() bool { return e.hasEvent(runnerproto.EventTurnEnded) })

	end, _ := e.turnEnded()
	if end.Outcome != runnerproto.OutcomeDone || end.Result != "all done" {
		t.Fatalf("turn ended %+v", end)
	}
	ev, ok := e.find(runnerproto.EventHarnessSession, nil)
	if !ok {
		t.Fatal("no harness.session event")
	}
	if d := decode[runnerproto.HarnessSessionData](t, ev); d.SessionID != "sess_1" {
		t.Fatalf("session %+v", d)
	}
	ev, ok = e.find(runnerproto.EventUsage, nil)
	if !ok {
		t.Fatal("no usage event")
	}
	if d := decode[runnerproto.UsageData](t, ev); !d.Cumulative || d.InputTokens != 100 || d.OutputTokens != 20 || d.Coverage != "known" {
		t.Fatalf("usage %+v", d)
	}
	ev, ok = e.find(runnerproto.EventMessage, nil)
	if !ok {
		t.Fatal("no message event")
	}
	if d := decode[runnerproto.MessageData](t, ev); d.Content != "token file: s3cret-file" {
		t.Fatalf("message %+v", d)
	}

	// The secret file is on disk with mode 0600.
	p := filepath.Join(e.work, "secrets", "token.txt")
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "s3cret-file" {
		t.Fatalf("secret file %q %v", b, err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("secret file mode %v %v", fi, err)
		}
	}

	// The harness got the secrets, the headers, the skills and the resume data.
	select {
	case r := <-h.seen:
		if r.Auth["EXAMPLE_API_KEY"] != "key-1" || r.MCPServers["srv"].Headers["X-Token"] != "h1" ||
			len(r.Skills) != 1 || r.Skills[0].Content != "be kind" || r.SystemPrompt != "sys" ||
			r.Harness.Model != "m" || r.RunID != e.run.ID || r.Resume {
			t.Fatalf("request %+v", r)
		}
	default:
		t.Fatal("harness did not run")
	}

	e.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{Reason: "shutdown"})
	if err := waitResult(t, done, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	e.wantEnded("stopped")
}

func TestSecretFileRejected(t *testing.T) {
	for _, path := range []string{"/etc/x", "../x", "a/../../x"} {
		e := newEnv(t, runnerproto.StartResponse{}, &runnerproto.SecretsResponse{
			Files: []runnerproto.SecretFile{{Path: path, ContentBase64: "eA=="}},
		})
		done := e.start(context.Background(), e.config(execHarness("cat")))
		if err := waitResult(t, done, 10*time.Second); err != nil {
			t.Fatal(err)
		}
		s := e.wantEnded("setup_failed")
		if s.FailureCode != string(rtcore.FailureEnvironmentSetupFailed) {
			t.Fatalf("%s: failure code %q", path, s.FailureCode)
		}
	}
}
