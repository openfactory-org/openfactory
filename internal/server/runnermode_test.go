package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/api"
	"github.com/openfactory-org/openfactory/internal/auth"
	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

var (
	runnerBinOnce sync.Once
	runnerBinPath string
	runnerBinErr  error
	runnerBinSkip bool
	runnerBinDir  string
)

func TestMain(m *testing.M) {
	code := m.Run()
	if runnerBinDir != "" {
		_ = os.RemoveAll(runnerBinDir)
	}
	os.Exit(code)
}

// runnerBinary builds the openfactory binary once and returns its path.
func runnerBinary(t *testing.T) string {
	t.Helper()
	runnerBinOnce.Do(func() {
		goBin, err := exec.LookPath("go")
		if err != nil {
			runnerBinSkip = true
			runnerBinErr = err
			return
		}
		out, err := exec.Command(goBin, "env", "GOMOD").Output()
		if err != nil {
			runnerBinErr = fmt.Errorf("go env GOMOD: %w", err)
			return
		}
		root := filepath.Dir(strings.TrimSpace(string(out)))
		dir, err := os.MkdirTemp("", "of-runner-bin-")
		if err != nil {
			runnerBinErr = err
			return
		}
		runnerBinDir = dir
		bin := filepath.Join(dir, "openfactory")
		// The control plane refuses the version "dev" (major version check), so the test binary sets one.
		cmd := exec.Command(goBin, "build", "-ldflags", "-X main.version=1.0.0-test", "-o", bin, "./cmd/openfactory")
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			runnerBinErr = fmt.Errorf("go build: %w\n%s", err, b)
			return
		}
		runnerBinPath = bin
	})
	if runnerBinSkip {
		t.Skipf("go is not on PATH: %v", runnerBinErr)
	}
	if runnerBinErr != nil {
		t.Fatal(runnerBinErr)
	}
	return runnerBinPath
}

// runnerServer starts a server in runner mode with the exec harness set to cat.
func runnerServer(t *testing.T) *Server {
	t.Helper()
	return runnerServerWith(t, nil)
}

// runnerServerWith is runnerServer with a hook that edits the config. The hook
// gets the public URL already set.
func runnerServerWith(t *testing.T, edit func(*config.Config)) *Server {
	t.Helper()
	bin := runnerBinary(t)
	ctx := context.Background()
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.DatabaseURL = "sqlite://:memory:"
	c.SandboxBackend = config.BackendLocal
	c.SandboxRoot = t.TempDir()
	c.SecretKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	c.RunnerCommand = bin + " runner"
	c.RunnerExecCommand = "cat"
	ts := httptest.NewUnstartedServer(nil)
	c.PublicURL = "http://" + ts.Listener.Addr().String()
	if edit != nil {
		edit(&c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, &c, WithLogger(quietLogger()), WithPollInterval(100*time.Millisecond), WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.Start()
	if err := srv.Start(ctx); err != nil {
		ts.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Close the server first: it stops the runner processes. A runner keeps
		// a command long poll open, so closing the HTTP server first blocks
		// until that poll ends (about 25 seconds per test).
		_ = srv.Close()
		ts.Close()
		// No runner process may stay alive.
		list, err := srv.runner.backend.List(ctx)
		if err != nil {
			t.Errorf("list tasks: %v", err)
		}
		for _, st := range list {
			if st.State != core.TaskExited {
				t.Errorf("task %s is still %s after Close", st.TaskID, st.State)
			}
		}
	})
	if err := srv.Store.CreateTeam(ctx, &core.Team{ID: "team1", Name: "team"}); err != nil {
		t.Fatal(err)
	}
	return srv
}

// startRun creates a factory with one agent, cfg, and starts a run of that
// agent. Each run belongs to a factory.
func startRun(ctx context.Context, srv *Server, cfg core.AgentConfig, prompt string) (*core.Run, error) {
	f := &core.Factory{TeamID: "team1", Name: "Factory " + cfg.Name, Alias: core.NewID("bot"), Source: core.DefinitionManaged}
	if err := srv.Store.CreateFactory(ctx, f); err != nil {
		return nil, err
	}
	cfg.ID, cfg.FactoryID = core.NewID("agt"), f.ID
	if err := srv.Store.ReplaceFactoryResources(ctx, f.ID, []core.AgentConfig{cfg}, nil, nil, nil); err != nil {
		return nil, err
	}
	_, run, err := srv.Engine.Dispatch(ctx, core.DispatchRequest{FactoryID: f.ID, AgentName: cfg.Name, Prompt: prompt})
	return run, err
}

func waitRun(t *testing.T, srv *Server, runID, what string, ok func(*core.Run) bool) *core.Run {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last *core.Run
	for time.Now().Before(deadline) {
		r, err := srv.Store.GetRun(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		last = r
		if ok(r) {
			return r
		}
		time.Sleep(50 * time.Millisecond)
	}
	dumpRunnerLogs(t, srv)
	t.Fatalf("timed out waiting for %s: state %s result %q error %q", what, last.State, last.Result, last.Error)
	return nil
}

func sessions(t *testing.T, srv *Server, runID string) []core.RunSession {
	t.Helper()
	list, err := srv.Store.(core.RunnerStore).ListRunSessions(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestRunnerModeConversation(t *testing.T) {
	srv := runnerServer(t)
	ctx := context.Background()
	cfg := core.AgentConfig{Name: "echo", Harness: core.HarnessConfig{Type: core.HarnessClaude}}
	run, err := startRun(ctx, srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}

	// Turn 1.
	r := waitRun(t, srv, run.ID, "first turn", func(r *core.Run) bool { return r.State == core.RunSucceeded })
	if r.Result != "hello" {
		t.Fatalf("result = %q", r.Result)
	}
	msgs, err := srv.Store.ListRunMessages(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range msgs {
		found = found || (m.Role == "assistant" && m.Content == "hello")
	}
	if !found {
		t.Errorf("no assistant message hello in %+v", msgs)
	}
	ss := sessions(t, srv, run.ID)
	if len(ss) != 1 || ss[0].State != core.SessionActive {
		t.Fatalf("sessions = %+v", ss)
	}

	// Turn 2 in the same session.
	if err := srv.Engine.FollowUp(ctx, run.ID, "again", "user_1"); err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "second turn", func(r *core.Run) bool {
		return r.State == core.RunSucceeded && r.Result == "again"
	})
	ss = sessions(t, srv, run.ID)
	if len(ss) != 1 || ss[0].Session != 1 {
		t.Fatalf("the follow-up used another session: %+v", ss)
	}

	// Cancel.
	if err := srv.Engine.Cancel(ctx, run.ID, "user_1"); err != nil {
		t.Fatal(err)
	}
	// The turn is done, so the run stays succeeded (runstate refuses cancel
	// in a final state). Cancel still ends the live session.
	deadline := time.Now().Add(30 * time.Second)
	for {
		ss = sessions(t, srv, run.ID)
		if len(ss) == 1 && ss[0].State == core.SessionEnded {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not end: %+v", ss)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ss[0].EndReason != "cancelled" {
		t.Errorf("end reason = %q", ss[0].EndReason)
	}
	if r, _ := srv.Store.GetRun(ctx, run.ID); r.State != core.RunSucceeded {
		t.Errorf("run state = %s", r.State)
	}
}

// The start response gives the orchestration server with the session token,
// and the MCP path accepts that token.
func TestRunnerModeOrchestrationServer(t *testing.T) {
	srv := runnerServer(t)
	ctx := context.Background()
	cfg := core.AgentConfig{Name: "echo", Harness: core.HarnessConfig{Type: core.HarnessClaude}}
	run, err := startRun(ctx, srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "first turn", func(r *core.Run) bool { return r.State == core.RunSucceeded })
	ss := sessions(t, srv, run.ID)
	if len(ss) != 1 {
		t.Fatalf("sessions = %+v", ss)
	}
	r, err := srv.Store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	start, err := srv.runner.content.StartConfig(ctx, r, &ss[0])
	if err != nil {
		t.Fatal(err)
	}
	if sc := start.Workspace.Snapshot; !sc.Enabled || !sc.Restore || sc.SkipWorkspace || sc.MaxPartBytes != srv.cfg.SnapshotMaxBytes {
		t.Errorf("snapshot config = %+v, want enabled with limit %d", sc, srv.cfg.SnapshotMaxBytes)
	}
	off := *r
	off.Config.Snapshot = new(bool) // snapshot: false
	offStart, err := srv.runner.content.StartConfig(ctx, &off, &ss[0])
	if err != nil {
		t.Fatal(err)
	}
	if !offStart.Workspace.Snapshot.SkipWorkspace || !offStart.Workspace.Snapshot.Enabled {
		t.Errorf("snapshot false: config = %+v", offStart.Workspace.Snapshot)
	}
	f, ok := start.Harness.MCPServers[engine.OrchestrationServerName]
	if !ok {
		t.Fatalf("no %s server in %+v", engine.OrchestrationServerName, start.Harness.MCPServers)
	}
	if f.Auth != "session_token" || !strings.HasSuffix(f.URL, "/mcp/run/"+run.ID) {
		t.Errorf("server = %+v", f)
	}
	sec, err := srv.runner.content.Secrets(ctx, r, &ss[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sec.MCPHeaders[engine.OrchestrationServerName]; ok {
		t.Errorf("headers of the session token server are in the secrets: %v", sec.MCPHeaders)
	}
	master, err := srv.cfg.MasterKey()
	if err != nil {
		t.Fatal(err)
	}
	// The old run token: an HMAC of the run ID with the removed derived key.
	mac := hmac.New(sha256.New, config.DeriveKey(master, "openfactory/orchestration-server-key"))
	mac.Write([]byte("run:" + run.ID))
	oldTok := hex.EncodeToString(mac.Sum(nil))
	if strings.Contains(strings.Join(sec.Redact, ","), oldTok) {
		t.Errorf("the redact list holds the old run token")
	}

	// The MCP path accepts a session token. It refuses the old run token.
	sessTok, _, err := runnertoken.Mint(srv.runner.keys, runnertoken.Claims{Team: r.TeamID, Run: run.ID, Session: 1, Subject: "runner"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status := func(tok string) int {
		req := httptest.NewRequest(http.MethodGet, "/mcp/run/"+run.ID, nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if got := status(sessTok); got == http.StatusUnauthorized || got == http.StatusConflict {
		t.Errorf("session token: status %d", got)
	}
	// The path refuses a token that is not a session token.
	if got := status(oldTok); got != http.StatusUnauthorized {
		t.Errorf("old run token: status %d, want 401", got)
	}
	if got := status(""); got != http.StatusUnauthorized {
		t.Errorf("no token: status %d", got)
	}
}

// A run in a working state goes to cancelled and its session ends.
func TestRunnerModeCancelWorkingRun(t *testing.T) {
	srv := runnerServer(t)
	ctx := context.Background()
	cfg := core.AgentConfig{Name: "slow", Harness: core.HarnessConfig{Type: core.HarnessClaude}}
	// A setup command that takes long keeps the run in a working state.
	cfg.Machine = &core.MachineSpec{SetupCommands: []string{"sleep 60"}}
	run, err := startRun(ctx, srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "claimed run", func(r *core.Run) bool { return r.State == core.RunClaimed })
	if err := srv.Engine.Cancel(ctx, run.ID, "user_1"); err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "cancelled run", func(r *core.Run) bool { return r.State == core.RunCancelled })
	deadline := time.Now().Add(30 * time.Second)
	for {
		ss := sessions(t, srv, run.ID)
		if len(ss) == 1 && ss[0].State == core.SessionEnded {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not end: %+v", ss)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRunnerModeSetupFailure(t *testing.T) {
	srv := runnerServer(t)
	cfg := core.AgentConfig{
		Name:    "broken",
		Harness: core.HarnessConfig{Type: core.HarnessClaude},
		Machine: &core.MachineSpec{SetupCommands: []string{"exit 3"}},
	}
	run, err := startRun(context.Background(), srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	r := waitRun(t, srv, run.ID, "failed run", func(r *core.Run) bool { return r.State == core.RunFailed })
	// The code stays in failure_code and at the start of the error. The rest
	// names the step that failed.
	if r.FailureCode != core.FailureEnvironmentSetupFailed || !strings.HasPrefix(r.Error, "environment_setup_failed: step command:1 failed: exit status 3") {
		t.Errorf("failure code %q, error = %q", r.FailureCode, r.Error)
	}
}

// dumpRunnerLogs prints the log file of each runner task to help with failures.
func dumpRunnerLogs(t *testing.T, srv *Server) {
	t.Helper()
	logs, _ := filepath.Glob(filepath.Join(srv.cfg.SandboxRoot, "tasks", "*", "runner.log"))
	for _, p := range logs {
		b, _ := os.ReadFile(p)
		t.Logf("%s:\n%s", p, b)
	}
}

// A session of a run can upload a file through the control plane and the
// object exists in the store after the PUT.
// GET /runs/{id}/transcript lists the run messages that the dispatcher stored
// from the runner events. The runner redacts the events, so a secret value
// that the agent prints does not appear in the transcript.
func TestRunnerModeTranscript(t *testing.T) {
	const secret = "tr4nscript-secret-value-0xAB"
	srv := runnerServerWith(t, func(c *config.Config) {
		c.RunnerExecCommand = `sh -c 'echo "token=$TRANSCRIPT_TOKEN"; cat'`
	})
	ctx := context.Background()
	if _, err := srv.Secrets.Put(ctx, "team1", "", "TRANSCRIPT_TOKEN", core.SecretGeneric, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	u := &core.User{ID: core.NewID("usr"), TeamID: "team1", Email: "a@example.com", Name: "a", Role: core.RoleAdmin, CreatedAt: time.Now().UTC()}
	if err := srv.Store.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	key, plain, err := api.NewAPIKey("team1", u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	cfg := core.AgentConfig{Name: "echo", Harness: core.HarnessConfig{Type: core.HarnessClaude}, Secrets: []string{"TRANSCRIPT_TOKEN"}}
	run, err := startRun(ctx, srv, cfg, "say hello")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "first turn", func(r *core.Run) bool { return r.State == core.RunSucceeded })

	req, err := http.NewRequest(http.MethodGet, srv.cfg.PublicURL+"/api/v1/runs/"+run.ID+"/transcript", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+plain)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("transcript = %d %s: %s", resp.StatusCode, resp.Header.Get("Content-Type"), body.String())
	}
	if strings.Contains(body.String(), secret) {
		t.Errorf("the transcript has the secret value:\n%s", body.String())
	}
	var roles []string
	var all string
	for _, l := range strings.Split(strings.TrimSpace(body.String()), "\n") {
		var m core.RunMessage
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		roles = append(roles, m.Role)
		all += m.Content + "\n"
	}
	if !strings.Contains(all, "token=***") {
		t.Errorf("the transcript has no redacted output (roles %v):\n%s", roles, body.String())
	}
	stored, err := srv.Store.ListRunMessages(ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != len(stored) {
		t.Errorf("transcript has %d messages, the database has %d", len(roles), len(stored))
	}
}

func TestRunnerModeUpload(t *testing.T) {
	srv := runnerServer(t)
	ctx := context.Background()
	cfg := core.AgentConfig{Name: "echo", Harness: core.HarnessConfig{Type: core.HarnessClaude}}
	run, err := startRun(ctx, srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "first turn", func(r *core.Run) bool { return r.State == core.RunSucceeded })
	ss := sessions(t, srv, run.ID)
	if len(ss) != 1 || ss[0].State != core.SessionActive {
		t.Fatalf("sessions = %+v", ss)
	}
	tok, _, err := runnertoken.Mint(srv.runner.keys, runnertoken.Claims{
		Version: runnertoken.SessionVersion, Team: "team1", Run: run.ID, Session: ss[0].Session, Subject: "runner",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	c, err := runnerclient.New(runnerclient.Options{BaseURL: srv.cfg.PublicURL, RunnerVersion: "1.0.0-test"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetToken(tok)

	data := []byte("artifact bytes")
	sum := sha256.Sum256(data)
	up, err := c.RequestUpload(ctx, runnerproto.UploadRequest{
		Kind: runnerproto.UploadArtifact, Name: "out/a.txt", Bytes: int64(len(data)),
		SHA256: hex.EncodeToString(sum[:]), ContentType: "text/plain",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PutFile(ctx, up, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	key := "runs/" + run.ID + "/sessions/1/artifact/out/a.txt"
	info, err := srv.RunData.Store(objectstore.Artifacts).Stat(ctx, key)
	if err != nil {
		t.Fatalf("object %s: %v", key, err)
	}
	if info.Size != int64(len(data)) {
		t.Errorf("size = %d", info.Size)
	}
}

func TestRedactList(t *testing.T) {
	got := redactList([]string{"short", "longer-secret", "short", ""})
	want := map[string]bool{"short": true, "longer-secret": true, "bG9uZ2VyLXNlY3JldA==": true}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected %q", g)
		}
	}
}

func TestRunnerIdleTimeoutSeconds(t *testing.T) {
	c := &runnerContent{idleDefault: 10 * time.Minute, idleMax: 30 * time.Minute}
	for _, tc := range []struct {
		agent int
		set   bool
		want  int
	}{
		{0, false, 600},  // the agent set none: the default
		{0, true, 0},     // the agent set 0: end the session after the turn
		{5, true, 300},   // the agent value
		{45, true, 1800}, // capped at the maximum
	} {
		if got := c.idleTimeoutSeconds(tc.agent, tc.set); got != tc.want {
			t.Errorf("idleTimeoutSeconds(%d, %v) = %d, want %d", tc.agent, tc.set, got, tc.want)
		}
	}
	c.idleDefault = 40 * time.Minute
	if got := c.idleTimeoutSeconds(0, false); got != 1800 {
		t.Errorf("default above the maximum = %d, want 1800", got)
	}
}

// With OF_INTERNAL_URL set, the MCP URLs in the start config use it. The
// public URL stays for links that people open.
func TestRunnerModeInternalURL(t *testing.T) {
	var internal string
	srv := runnerServerWith(t, func(c *config.Config) {
		// Same listener, other host name: the runner can still reach it.
		internal = strings.Replace(c.PublicURL, "127.0.0.1", "localhost", 1)
		c.InternalURL = internal + "/"
	})
	if internal == srv.cfg.PublicURL {
		t.Skip("the listener is not on 127.0.0.1")
	}
	ctx := context.Background()
	cfg := core.AgentConfig{Name: "echo", Harness: core.HarnessConfig{Type: core.HarnessClaude}}
	run, err := startRun(ctx, srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "first turn", func(r *core.Run) bool { return r.State == core.RunSucceeded })
	ss := sessions(t, srv, run.ID)
	r, err := srv.Store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	start, err := srv.runner.content.StartConfig(ctx, r, &ss[0])
	if err != nil {
		t.Fatal(err)
	}
	f, ok := start.Harness.MCPServers[engine.OrchestrationServerName]
	if !ok {
		t.Fatalf("no orchestration server in %+v", start.Harness.MCPServers)
	}
	if want := internal + "/mcp/run/" + run.ID; f.URL != want {
		t.Errorf("orchestration URL = %q, want %q", f.URL, want)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	events []auth.AuditEvent
}

func (r *recordingSink) Record(_ context.Context, e auth.AuditEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// A secrets read writes one audit event. It holds names and no values.
func TestSecretsReadEvent(t *testing.T) {
	const value = "sk-super-secret-value"
	run := &core.Run{ID: "run_1", TeamID: "team1"}
	resp := runnerproto.SecretsResponse{
		Env:         map[string]string{"DB_PASSWORD": value},
		Files:       []runnerproto.SecretFile{{Path: "/home/agent/.netrc", ContentBase64: value}},
		HarnessAuth: map[string]string{"ANTHROPIC_API_KEY": value},
		MCPHeaders:  map[string]map[string]string{"linear": {"Authorization": "Bearer " + value}},
		MCPEnv:      map[string]map[string]string{"db": {"TOKEN": value}},
		Redact:      []string{value},
	}
	ev := secretsReadEvent(run, &core.RunSession{Session: 3}, resp)
	if ev.Action != "runner.secrets.read" || ev.Outcome != auth.OutcomeSuccess || ev.ActorID != "run:run_1" || ev.TeamID != "team1" {
		t.Errorf("event = %+v", ev)
	}
	if ev.Resource == nil || ev.Resource.Type != "run" || ev.Resource.ID != "run_1" {
		t.Errorf("resource = %+v", ev.Resource)
	}
	want := map[string]string{"session": "3", "env": "DB_PASSWORD", "files": "/home/agent/.netrc", "harness_auth": "ANTHROPIC_API_KEY", "mcp_servers": "db,linear", "team_id": "team1"}
	for k, v := range want {
		if ev.Detail[k] != v {
			t.Errorf("detail[%s] = %q, want %q", k, ev.Detail[k], v)
		}
	}
	if b, _ := json.Marshal(ev); strings.Contains(string(b), value) {
		t.Errorf("the event holds a secret value: %s", b)
	}
}

// The runner secrets call records exactly one event in the audit sink.
func TestRunnerSecretsAudit(t *testing.T) {
	srv := runnerServer(t)
	ctx := context.Background()
	cfg := core.AgentConfig{Name: "echo", Harness: core.HarnessConfig{Type: core.HarnessClaude}}
	run, err := startRun(ctx, srv, cfg, "hello")
	if err != nil {
		t.Fatal(err)
	}
	waitRun(t, srv, run.ID, "first turn", func(r *core.Run) bool { return r.State == core.RunSucceeded })
	ss := sessions(t, srv, run.ID)
	r, err := srv.Store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	srv.runner.content.audit = sink
	if _, err := srv.runner.content.Secrets(ctx, r, &ss[0]); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 || sink.events[0].Action != "runner.secrets.read" || sink.events[0].Resource.ID != run.ID {
		t.Fatalf("events = %+v", sink.events)
	}
}
