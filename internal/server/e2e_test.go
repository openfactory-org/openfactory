package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/forge/gitgeneric"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
	"github.com/openfactory-org/openfactory/internal/webhooks"
	"github.com/openfactory-org/openfactory/pkg/sdk"
)

// ---------- test doubles ----------

// fileForges resolves every code forge to a generic git forge serving bare
// repositories from a local directory (file:// URLs).
type fileForges struct{ f core.Forge }

func (r fileForges) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return r.f, nil }

// headerRT adds the Authorization header of the runner to each request.
type headerRT struct{ token string }

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+h.token)
	return http.DefaultTransport.RoundTrip(r)
}

// markRunning sets the state of a run to running, as it is during a turn.
func markRunning(t *testing.T, srv *Server, runID string) {
	t.Helper()
	for range 5 {
		r, err := srv.Store.GetRun(t.Context(), runID)
		if err != nil {
			t.Fatal(err)
		}
		r.State, r.FinishedAt = core.RunRunning, nil
		err = srv.Store.UpdateRun(t.Context(), r)
		if err == nil {
			return
		}
		if !errors.Is(err, core.ErrConflict) {
			t.Fatal(err)
		}
	}
	t.Fatalf("could not set run %s to running", runID)
}

// foremanTool calls a tool of the orchestration MCP server for a run, with the
// session token that the runner of the run would use. The test plays the
// foreman: the exec harness of the runner only echoes its input.
func foremanTool(t *testing.T, srv *Server, baseURL, runID, name string, args any) string {
	t.Helper()
	ctx := t.Context()
	rs := srv.Store.(core.RunnerStore)
	sess, err := rs.LatestRunSession(ctx, runID)
	if err != nil {
		t.Fatalf("session of %s: %v", runID, err)
	}
	run, err := srv.Store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := runnertoken.Mint(srv.runner.keys, runnertoken.Claims{Team: run.TeamID, Run: runID, Session: sess.Session, Subject: "runner"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-foreman", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             baseURL + "/mcp/run/" + runID,
		HTTPClient:           &http.Client{Transport: headerRT{tok}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("connect to the orchestration server as %s: %v", runID, err)
	}
	defer cs.Close()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	if res.IsError {
		t.Fatalf("tool %s returned an error: %s", name, b.String())
	}
	return b.String()
}

// ---------- helpers ----------

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return string(out)
}

// makeRepo creates <root>/acme/app.git with one commit on main.
func makeRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work := t.TempDir()
	run(t, work, "git", "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hello factory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "git", "add", ".")
	run(t, work, "git", "commit", "-q", "-m", "init")
	bare := filepath.Join(root, "acme", "app.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, root, "git", "clone", "-q", "--bare", work, bare)
	return root
}

func waitFor(t *testing.T, what string, timeout time.Duration, fn func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := fn()
		if ok {
			return
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s (last error: %v)", what, lastErr)
}

// ---------- the end-to-end flow ----------

func TestEndToEndWebhookToCompletion(t *testing.T) {
	ctx := t.Context()
	repoRoot := makeRepo(t)
	fg, err := gitgeneric.New(gitgeneric.Config{BaseURL: "file://" + repoRoot})
	if err != nil {
		t.Fatal(err)
	}

	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.DatabaseURL = "sqlite://:memory:"
	c.SandboxBackend = config.BackendLocal
	c.SandboxRoot = t.TempDir()
	c.SecretKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	// The runner of each task runs the exec harness. It prints the README of
	// the checked-out repository, the metadata of the run and its input.
	c.RunnerCommand = runnerBinary(t) + " runner"
	c.RunnerExecCommand = `sh -c 'cat README.md; echo ticket=$OF_METADATA_TICKET; cat'`

	ts := httptest.NewUnstartedServer(nil)
	c.PublicURL = "http://" + ts.Listener.Addr().String()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := New(ctx, &c,
		WithLogger(quietLogger()),
		WithForgeResolver(fileForges{fg}),
		WithPollInterval(100*time.Millisecond),
		WithDefinitionSyncInterval(-1),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.Start()
	defer ts.Close()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Bootstrap the first team and admin through the public endpoint.
	anon := sdk.NewClient(ts.URL, "", sdk.WithMaxRetries(0))
	boot, err := anon.Platform.Bootstrap(ctx, "Acme", "admin@example.com", "Admin")
	if err != nil {
		t.Fatal(err)
	}
	cl := sdk.NewClient(ts.URL, boot.Key, sdk.WithMaxRetries(0))

	const whSecret = "e2e-webhook-signing-secret"
	if _, err := cl.Secrets.Create(ctx, "INCOMING_WEBHOOK_SECRET", "generic", whSecret, ""); err != nil {
		t.Fatal(err)
	}

	// The default definition (foreman + implement) plus a signed custom webhook
	// and an automation that routes its deliveries to the foreman.
	files, err := definition.DefaultDefinition(definition.DefaultDefinitionOptions{
		Name:         "E2E Factory",
		Repositories: []core.RepoRef{{CodeForge: "GITHUB", Owner: "acme", Name: "app"}},
		Agents:       []string{"implement"},
	})
	if err != nil {
		t.Fatal(err)
	}
	files["webhooks/incoming.yaml"] = "authMode: signature\nsignatureScheme: github\nsecretName: INCOMING_WEBHOOK_SECRET\n"
	files["automations/on-alert/automation.md"] = `---
triggers:
  - provider: webhook
    event: received
    filter:
      webhook_ids: [incoming]
      payload:
        action: [fire]
---

An alert fired. Handle it.
`
	created, err := cl.Factories.Create(ctx, sdk.CreateFactoryParams{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	fid := created.Factory.UID
	hooks, err := cl.Webhooks.List(ctx, fid)
	if err != nil || len(hooks) != 1 {
		t.Fatalf("webhooks: %v %+v", err, hooks)
	}

	// A delivery that does not match the payload filter starts nothing.
	deliver := func(body string, id string) int {
		hdr, err := webhooks.Sign("github", []byte(whSecret), []byte(body), time.Now(), id)
		if err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("POST", ts.URL+"/webhooks/w/"+hooks[0].ID, bytes.NewReader([]byte(body)))
		for k, v := range hdr {
			req.Header[k] = v
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := deliver(`{"action":"ignore"}`, "d-0"); code != http.StatusAccepted {
		t.Fatalf("delivery status %d", code)
	}
	// Unsigned deliveries are rejected.
	resp, err := http.Post(ts.URL+"/webhooks/w/"+hooks[0].ID, "application/json", strings.NewReader(`{"action":"fire"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("unsigned delivery accepted: %d", resp.StatusCode)
	}
	if code := deliver(`{"action":"fire","alert":"disk full"}`, "d-1"); code != http.StatusAccepted {
		t.Fatalf("delivery status %d", code)
	}

	// The foreman run starts in a runner. The test plays the foreman: it calls
	// the tools of the orchestration server with the session token of the run.
	var foreman *core.Run
	waitFor(t, "foreman run", 30*time.Second, func() (bool, error) {
		list, err := srv.Store.ListRuns(ctx, core.RunFilter{})
		if err != nil {
			return false, err
		}
		for i := range list {
			if list[i].AgentName == "foreman" && list[i].State == core.RunSucceeded {
				foreman = &list[i]
				return true, nil
			}
		}
		return false, fmt.Errorf("runs: %d", len(list))
	})
	if !strings.Contains(foreman.Result, "An alert fired") {
		t.Fatalf("the foreman got no prompt from the automation: %q", foreman.Result)
	}
	var calls []string
	call := func(name string, args any) string {
		// A real harness calls its tools during a turn, while the run is
		// running. The exec harness has ended its turn, so the test sets the state.
		markRunning(t, srv, foreman.ID)
		out := foremanTool(t, srv, ts.URL, foreman.ID, name, args)
		calls = append(calls, name)
		return out
	}
	call("run_agents", map[string]any{
		"summary": "Implement the requested change",
		"agent_run_configs": []map[string]any{{
			"name": "build", "agent_identity_uid": "implement", "prompt": "Add CHANGE.md describing the alert.",
			"metadata": map[string]string{"ticket": "INC-1"},
		}},
	})

	// The child runs in its own runner. Its result shows the checked-out
	// repository and the metadata of the run.
	var child *core.Run
	waitFor(t, "child run", 30*time.Second, func() (bool, error) {
		list, err := srv.Store.ListRuns(ctx, core.RunFilter{})
		if err != nil {
			return false, err
		}
		for i := range list {
			if list[i].Name == "build" {
				if list[i].State == core.RunFailed || list[i].State == core.RunError {
					return false, fmt.Errorf("child run %s: %s", list[i].State, list[i].Error)
				}
				if list[i].State == core.RunSucceeded {
					child = &list[i]
					return true, nil
				}
			}
		}
		return false, fmt.Errorf("runs: %d", len(list))
	})
	if !strings.Contains(child.Result, "hello factory") || !strings.Contains(child.Result, "ticket=INC-1") {
		t.Fatalf("child result does not show the repository and the metadata: %q", child.Result)
	}
	full, err := cl.Runs.Get(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fmt.Sprint(full), "hello factory") {
		t.Fatalf("API run does not show the checked-out repository: %+v", full)
	}

	// The foreman asks for approval.
	call("request_approval", map[string]any{"title": "Ship the change?", "summary": "The implement agent finished."})
	var inboxID string
	waitFor(t, "approval request", 30*time.Second, func() (bool, error) {
		items, err := cl.Inbox.List(ctx, nil).Collect(ctx)
		if err != nil {
			return false, err
		}
		for _, it := range items {
			if it.Kind == "approval" && !it.Resolved {
				inboxID = it.ID
				return true, nil
			}
			if it.Kind == "failure" {
				return false, fmt.Errorf("failure inbox item: %s", it.Body)
			}
		}
		return false, fmt.Errorf("inbox: %+v", items)
	})

	// Approve through the inbox API; the foreman completes the work item.
	if _, err := cl.Inbox.Resolve(ctx, inboxID, "approve"); err != nil {
		t.Fatal(err)
	}
	call("complete_work_item", map[string]any{"note": "Change implemented and approved."})
	waitFor(t, "work item completion", 30*time.Second, func() (bool, error) {
		items, err := cl.WorkItems.List(ctx, fid, nil).Collect(ctx)
		if err != nil {
			return false, fmt.Errorf("work items: %w", err)
		}
		if len(items) != 1 {
			return false, fmt.Errorf("work items: %+v", items)
		}
		return items[0].Stage == "complete", fmt.Errorf("stage %s", items[0].Stage)
	})
	if got := strings.Join(calls, ","); got != "run_agents,request_approval,complete_work_item" {
		t.Fatalf("orchestration calls = %s", got)
	}
	deliveries, err := cl.Webhooks.Deliveries(ctx, hooks[0].ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for _, d := range deliveries {
		if d.Status == "accepted" {
			accepted++
		}
	}
	if accepted != 2 {
		t.Fatalf("deliveries: %+v", deliveries)
	}

	// Each run has run messages: the runner sent the output of its turns.
	storedRuns, err := srv.Store.ListRuns(ctx, core.RunFilter{})
	if err != nil || len(storedRuns) < 2 {
		t.Fatalf("runs: %v %d", err, len(storedRuns))
	}
	for _, r := range storedRuns {
		msgs, err := srv.Store.ListRunMessages(ctx, r.ID, 0)
		if err != nil || len(msgs) == 0 {
			t.Errorf("run %s (%s): no messages (%v)", r.ID, r.AgentName, err)
		}
	}

	// Metrics reflect the finished work.
	var body bytes.Buffer
	if code := get(t, ts.URL+"/metrics", &body); code != 200 || !strings.Contains(body.String(), "of_runs{") {
		t.Fatalf("metrics: %d %s", code, body.String())
	}
}

// TestValidateExamplesBinary builds the CLI and validates every example.
func TestValidateExamplesBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	_, file, _, _ := runtime.Caller(0)
	repo := filepath.Join(filepath.Dir(file), "..", "..")
	bin := filepath.Join(t.TempDir(), "openfactory")
	build := exec.Command("go", "build", "-o", bin, "./cmd/openfactory")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	examples, err := filepath.Glob(filepath.Join(repo, "examples", "*"))
	if err != nil || len(examples) == 0 {
		t.Fatalf("no examples: %v", err)
	}
	for _, ex := range examples {
		if _, err := os.Stat(filepath.Join(ex, "factory.yaml")); err != nil {
			continue // not a definition tree (e.g. the custom-binary Go module)
		}
		out, err := exec.Command(bin, "validate", ex).CombinedOutput()
		if err != nil {
			t.Errorf("validate %s: %v\n%s", filepath.Base(ex), err, out)
		}
	}
	// An invalid tree exits 1 and reports file:line.
	bad := t.TempDir()
	_ = os.WriteFile(filepath.Join(bad, "factory.yaml"), []byte("schemaVersion: v1\nname: x\nbogusField: 1\n"), 0o644)
	cmd := exec.Command(bin, "validate", bad)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 1 {
		t.Fatalf("invalid definition: err=%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "factory.yaml:") {
		t.Fatalf("expected file:line in output:\n%s", out)
	}
}
