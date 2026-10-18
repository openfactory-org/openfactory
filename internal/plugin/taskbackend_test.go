package plugin

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/pkg/plugin/sdk"
)

// fakeTasks is a task backend that a plugin serves in the tests.
type fakeTasks struct {
	mu      sync.Mutex
	started map[string]core.Task
	calls   []string
}

func (f *fakeTasks) Name() string { return "fake-tasks" }

func (f *fakeTasks) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeTasks) Start(_ context.Context, t core.Task) (core.TaskStatus, error) {
	f.record("start " + t.TaskID)
	if t.TaskID == "full.1" {
		return core.TaskStatus{}, core.ErrAtCapacity
	}
	if err := t.Validate(); err != nil {
		return core.TaskStatus{}, fmt.Errorf("%w: %w", core.ErrInvalid, err)
	}
	f.mu.Lock()
	f.started[t.TaskID] = t
	f.mu.Unlock()
	return core.TaskStatus{TaskID: t.TaskID, State: core.TaskAccepted}, nil
}

func (f *fakeTasks) Status(_ context.Context, id string) (core.TaskStatus, error) {
	f.record("status " + id)
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.started[id]; !ok {
		return core.TaskStatus{}, fmt.Errorf("task %s: %w", id, core.ErrNotFound)
	}
	start := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	code := 3
	return core.TaskStatus{TaskID: id, State: core.TaskExited, Reason: core.SandboxProcessFailed, ExitCode: &code,
		Message: "boom", SandboxID: "sb-1", StartedAt: &start, EndedAt: &end}, nil
}

func (f *fakeTasks) Watch(ctx context.Context, id string) (<-chan core.TaskStatus, error) {
	f.record("watch " + id)
	f.mu.Lock()
	_, ok := f.started[id]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("task %s: %w", id, core.ErrNotFound)
	}
	ch := make(chan core.TaskStatus, 2)
	ch <- core.TaskStatus{TaskID: id, State: core.TaskRunning}
	ch <- core.TaskStatus{TaskID: id, State: core.TaskExited, Reason: core.SandboxCompleted}
	close(ch)
	return ch, nil
}

func (f *fakeTasks) Cancel(_ context.Context, id string, grace int) error {
	f.record(fmt.Sprintf("cancel %s %d", id, grace))
	return nil
}

func (f *fakeTasks) Destroy(_ context.Context, id string) error {
	f.record("destroy " + id)
	return nil
}

func (f *fakeTasks) List(context.Context) ([]core.TaskStatus, error) {
	return []core.TaskStatus{{TaskID: "a.1", State: core.TaskRunning}, {TaskID: "b.2", State: core.TaskAccepted}}, nil
}

func (f *fakeTasks) Preflight(context.Context) error { return errors.New("backend not ready") }

func testTask() core.Task {
	return core.Task{
		TaskID: "run_abc.2", RunID: "run_abc", Session: 2, TeamID: "team_1", FactoryID: "fac_1",
		Image: "img:1", RunnerImage: "runner:1",
		ToolImages: []core.TaskImage{{Name: "claude", Image: "cli:1"}},
		Platform:   core.TaskPlatform{OS: "linux", Arch: "amd64"},
		Resources:  core.TaskResources{CPU: "2", Memory: "8Gi"},
		Env: map[string]string{core.TaskEnvControlURL: "http://cp", core.TaskEnvRunID: "run_abc",
			core.TaskEnvSession: "2", core.TaskEnvBootstrapToken: "ofb_x"},
		NetworkPolicy:           &core.TaskNetworkPolicy{Mode: "restricted", AllowedHosts: []string{"a.example"}, DeniedHosts: []string{"b.example"}},
		MaxSessionSeconds:       600,
		FailureRetentionSeconds: 30,
		WorkspaceSizeLimit:      "20Gi",
		Labels:                  map[string]string{"k": "v"},
	}
}

func TestTaskBackendRoundTrip(t *testing.T) {
	env := newTestEnv(t, nil)
	tasks := &fakeTasks{started: map[string]core.Task{}}
	mk := func() sdk.Plugin { return sdk.Plugin{Name: "tb", Version: "0.1.0", SandboxBackend: tasks} }
	ctx := context.Background()
	if err := env.host.launch(ctx, "tb-plugin", testLauncher(t, mk, &env.launches, &env.kills)); err != nil {
		t.Fatal(err)
	}
	b, err := env.host.TaskBackend("tb")
	if err != nil {
		t.Fatal(err)
	}
	if b.Name() != "plugin:tb" {
		t.Fatalf("name = %q", b.Name())
	}

	task := testTask()
	st, err := b.Start(ctx, task)
	if err != nil || st.State != core.TaskAccepted || st.TaskID != "run_abc.2" {
		t.Fatalf("start = %+v, %v", st, err)
	}
	tasks.mu.Lock()
	got := tasks.started["run_abc.2"]
	tasks.mu.Unlock()
	if !reflect.DeepEqual(got, task) {
		t.Fatalf("task changed on the wire:\n got %+v\nwant %+v", got, task)
	}

	st, err = b.Status(ctx, "run_abc.2")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != core.TaskExited || st.Reason != core.SandboxProcessFailed || st.ExitCode == nil || *st.ExitCode != 3 ||
		st.Message != "boom" || st.SandboxID != "sb-1" || st.StartedAt == nil || st.EndedAt == nil ||
		!st.EndedAt.Equal(st.StartedAt.Add(time.Minute)) {
		t.Fatalf("status = %+v", st)
	}

	ch, err := b.Watch(ctx, "run_abc.2")
	if err != nil {
		t.Fatal(err)
	}
	var seen []core.TaskState
	for s := range ch {
		seen = append(seen, s.State)
	}
	if len(seen) != 2 || seen[0] != core.TaskRunning || seen[1] != core.TaskExited {
		t.Fatalf("watch statuses = %v", seen)
	}

	if err := b.Cancel(ctx, "run_abc.2", 15); err != nil {
		t.Fatal(err)
	}
	if err := b.Destroy(ctx, "run_abc.2"); err != nil {
		t.Fatal(err)
	}
	list, err := b.List(ctx)
	if err != nil || len(list) != 2 || list[1].TaskID != "b.2" {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if err := b.Preflight(ctx); err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("preflight = %v", err)
	}

	tasks.mu.Lock()
	calls := strings.Join(tasks.calls, ",")
	tasks.mu.Unlock()
	for _, want := range []string{"cancel run_abc.2 15", "destroy run_abc.2", "watch run_abc.2"} {
		if !strings.Contains(calls, want) {
			t.Errorf("calls %q lack %q", calls, want)
		}
	}

	t.Run("errors", func(t *testing.T) {
		if _, err := b.Status(ctx, "none.1"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("status of unknown task: %v", err)
		}
		if _, err := b.Watch(ctx, "none.1"); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("watch of unknown task: %v", err)
		}
		bad := task
		bad.Image = ""
		if _, err := b.Start(ctx, bad); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("start of invalid task: %v", err)
		}
		full := task
		full.TaskID, full.RunID, full.Session = "full.1", "full", 1
		if _, err := b.Start(ctx, full); !errors.Is(err, core.ErrAtCapacity) {
			t.Errorf("start at capacity: %v", err)
		}
	})
}

func TestTaskBackendNeedsV2Service(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	if err := env.host.launch(ctx, "fake-plugin", testLauncher(t, env.fakePlugin(nil), &env.launches, &env.kills)); err != nil {
		t.Fatal(err)
	}
	// The plugin "fake" serves no SandboxBackend.
	_, err := env.host.TaskBackend("fake")
	if err == nil || !strings.Contains(err.Error(), "plugin fake has no SandboxBackend service; the server needs plugin protocol v2") {
		t.Fatalf("error = %v", err)
	}
	if _, err := env.host.TaskBackend("missing"); err == nil || !strings.Contains(err.Error(), "not loaded") {
		t.Fatalf("missing plugin: %v", err)
	}
}

func TestPluginEnvOmitsServerSecrets(t *testing.T) {
	server := map[string]string{
		"PATH": "/bin", "HOME": "/home/x", "TMPDIR": "/tmp", "LANG": "C", "TZ": "UTC",
		"OF_SECRET_KEY": "k", "OF_SECRET_KEY_PREVIOUS": "k0", "OF_SIGNING_KEY": "s", "OF_SIGNING_KEY_PREVIOUS": "s0",
		"OF_DATABASE_URL": "postgres://x", "OF_ANTHROPIC_API_KEY": "a", "OPENAI_API_KEY": "o",
		"OF_OBJECT_STORE_ACCESS_KEY": "x", "SHELL": "/bin/zsh",
		"OF_GITLAB_WEBHOOK_TOKEN": "glt", "MY_PLUGIN_URL": "http://p",
	}
	lookup := func(n string) (string, bool) { v, ok := server[n]; return v, ok }
	toMap := func(env []string) map[string]string {
		m := map[string]string{}
		for _, kv := range env {
			k, v, _ := strings.Cut(kv, "=")
			m[k] = v
		}
		return m
	}

	got := toMap(pluginEnv("/p/openfactory-plugin-community", nil, nil, lookup))
	if len(got) != 5 {
		t.Fatalf("env = %v", got)
	}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "LANG", "TZ"} {
		if got[k] != server[k] {
			t.Errorf("%s = %q", k, got[k])
		}
	}

	// The operator allows a variable. Keys of the server stay out, also when listed.
	got = toMap(pluginEnv("/p/openfactory-plugin-community",
		[]string{"MY_PLUGIN_URL", " OF_SECRET_KEY ", "OF_SIGNING_KEY", "OF_DATABASE_URL", "OF_ANTHROPIC_API_KEY", "NOT_SET"}, nil, lookup))
	if got["MY_PLUGIN_URL"] != "http://p" {
		t.Errorf("allowed variable is missing: %v", got)
	}
	for _, k := range []string{"OF_SECRET_KEY", "OF_SECRET_KEY_PREVIOUS", "OF_SIGNING_KEY", "OF_SIGNING_KEY_PREVIOUS",
		"OF_DATABASE_URL", "OF_ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OF_OBJECT_STORE_ACCESS_KEY", "SHELL", "OF_GITLAB_WEBHOOK_TOKEN", "NOT_SET"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s is in the plugin environment", k)
		}
	}

	// A list for one plugin gives the variable to that plugin only. Keys of
	// the server stay out of it too.
	perPlugin := map[string][]string{"community": {"MY_PLUGIN_URL", "OF_SECRET_KEY"}}
	got = toMap(pluginEnv("/p/openfactory-plugin-community", nil, perPlugin, lookup))
	if got["MY_PLUGIN_URL"] != "http://p" {
		t.Errorf("per-plugin variable is missing: %v", got)
	}
	if _, ok := got["OF_SECRET_KEY"]; ok {
		t.Errorf("server key is in the plugin environment: %v", got)
	}
	got = toMap(pluginEnv("/p/openfactory-plugin-other", nil, perPlugin, lookup))
	if _, ok := got["MY_PLUGIN_URL"]; ok {
		t.Errorf("a list for one plugin reached another plugin: %v", got)
	}

	// A bundled plugin gets no extra variables: its settings come from
	// integration records.
	got = toMap(pluginEnv("/usr/lib/openfactory-plugin-gitlab", nil, nil, lookup))
	if _, ok := got["OF_GITLAB_WEBHOOK_TOKEN"]; ok || len(got) != 5 {
		t.Errorf("gitlab plugin env = %v", got)
	}
}

// TestPluginCmdEnvironment checks the command that the host starts: with the
// server key in the host process, the plugin command lacks it.
func TestPluginCmdEnvironment(t *testing.T) {
	t.Setenv("OF_SECRET_KEY", "super-secret")
	t.Setenv("OF_PLUGIN_TEST_ALLOWED", "yes")
	t.Setenv("OF_PLUGIN_TEST_ONLY_X", "x-only")
	h := New(Options{
		EnvAllow:    []string{"OF_PLUGIN_TEST_ALLOWED"},
		EnvByPlugin: map[string][]string{"x": {"OF_PLUGIN_TEST_ONLY_X"}},
	})
	t.Cleanup(func() { _ = h.Close() })
	cmd := pluginCmd(h, "/p/openfactory-plugin-x")
	var allowed, secret, only bool
	for _, kv := range cmd.Env {
		allowed = allowed || kv == "OF_PLUGIN_TEST_ALLOWED=yes"
		only = only || kv == "OF_PLUGIN_TEST_ONLY_X=x-only"
		secret = secret || strings.HasPrefix(kv, "OF_SECRET_KEY=")
	}
	if !allowed || !only || secret {
		t.Fatalf("allowed=%v only=%v secret=%v env=%v", allowed, only, secret, cmd.Env)
	}
	for _, kv := range pluginCmd(h, "/p/openfactory-plugin-y").Env {
		if strings.HasPrefix(kv, "OF_PLUGIN_TEST_ONLY_X=") {
			t.Fatalf("the list of plugin x reached plugin y: %v", kv)
		}
	}
	if _, ok := os.LookupEnv("OF_SECRET_KEY"); !ok {
		t.Fatal("test setup: the server key must be set")
	}
}
