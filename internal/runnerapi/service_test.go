package runnerapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
	"github.com/openfactory-org/openfactory/internal/store/sqlstore"
	"github.com/openfactory-org/openfactory/internal/store/testdb"
)

type fakeConfigurer struct{}

func (fakeConfigurer) StartConfig(context.Context, *core.Run, *core.RunSession) (runnerproto.StartResponse, error) {
	return runnerproto.StartResponse{ConversationID: "conv_1"}, nil
}

type fakeHooks struct {
	mu      sync.Mutex
	started int
	events  []core.RunnerEvent
	batches int
	ended   []runnerproto.EndRequest
	ctxErrs []error // ctx.Err() of each EventsStored call
}

func (h *fakeHooks) SessionStarted(context.Context, *core.RunSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started++
}

func (h *fakeHooks) EventsStored(ctx context.Context, _ *core.RunSession, ev []core.RunnerEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.batches++
	h.events = append(h.events, ev...)
	h.ctxErrs = append(h.ctxErrs, ctx.Err())
}

func (h *fakeHooks) SessionEnded(_ context.Context, _ *core.RunSession, req runnerproto.EndRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ended = append(h.ended, req)
}

// clock is a clock that a test can move forward.
type clock struct{ offset atomic.Int64 }

func (c *clock) now() time.Time { return time.Now().Add(time.Duration(c.offset.Load())) }
func (c *clock) advance(d time.Duration) {
	c.offset.Add(int64(d))
}

type env struct {
	t     *testing.T
	mem   *sqlstore.Store
	svc   *Service
	srv   *httptest.Server
	hooks *fakeHooks
	clock *clock
	keys  runnertoken.KeySet
	run   *core.Run
	boot  string
}

func newEnv(t *testing.T, mod func(*Config)) *env {
	t.Helper()
	keys, err := runnertoken.NewStaticKeys("k1", map[string][]byte{"k1": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, mem: testdb.New(t), hooks: &fakeHooks{}, clock: &clock{}, keys: keys}
	cfg := Config{
		Store:        e.mem,
		Runner:       e.mem,
		Keys:         keys,
		Session:      fakeConfigurer{},
		Hooks:        e.hooks,
		Now:          e.clock.now,
		PollInterval: 50 * time.Millisecond,
	}
	if mod != nil {
		mod(&cfg)
	}
	e.svc, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.svc.Handler())
	t.Cleanup(e.srv.Close)
	e.run = &core.Run{FactoryID: "fac_test", TeamID: "team_1"}
	if err := e.mem.CreateRun(context.Background(), e.run); err != nil {
		t.Fatal(err)
	}
	e.boot = e.newSession(1, time.Hour)
	return e
}

// newSession creates a session of the run and returns its bootstrap token.
func (e *env) newSession(n int, ttl time.Duration) string {
	e.t.Helper()
	tok, hash, err := IssueBootstrap()
	if err != nil {
		e.t.Fatal(err)
	}
	s := &core.RunSession{
		RunID:              e.run.ID,
		Session:            n,
		TaskID:             "task",
		State:              core.SessionAssigned,
		BootstrapHash:      hash,
		BootstrapExpiresAt: time.Now().Add(ttl),
	}
	if err := e.mem.CreateRunSession(context.Background(), s); err != nil {
		e.t.Fatal(err)
	}
	return tok
}

func (e *env) client() *runnerclient.Client {
	e.t.Helper()
	c, err := runnerclient.New(runnerclient.Options{BaseURL: e.srv.URL, RunnerVersion: "1.0.0"})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func startReq(instance string) runnerproto.StartRequest {
	return runnerproto.StartRequest{
		InstanceID:    instance,
		RunnerVersion: "1.0.0",
		SandboxID:     "job-1",
		Platform:      runnerproto.Platform{OS: "linux", Arch: "amd64"},
	}
}

// started returns a client that finished session start.
func (e *env) started() *runnerclient.Client {
	e.t.Helper()
	c := e.client()
	if _, err := c.Start(context.Background(), e.boot, startReq("inst-1")); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) session(n int) *core.RunSession {
	e.t.Helper()
	s, err := e.mem.GetRunSession(context.Background(), e.run.ID, n)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func TestNewChecksConfig(t *testing.T) {
	e := newEnv(t, nil)
	good := Config{Store: e.mem, Runner: e.mem, Keys: e.keys, Session: fakeConfigurer{}}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"store":   func(c *Config) { c.Store = nil },
		"runner":  func(c *Config) { c.Runner = nil },
		"keys":    func(c *Config) { c.Keys = nil },
		"session": func(c *Config) { c.Session = nil },
	} {
		c := good
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestStartFirstCall(t *testing.T) {
	e := newEnv(t, nil)
	resp, err := e.client().Start(context.Background(), e.boot, startReq("inst-1"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.SessionToken == "" || resp.RunID != e.run.ID || resp.Session != 1 || resp.ConversationID != "conv_1" {
		t.Fatalf("bad response: %+v", resp)
	}
	s := e.session(1)
	if s.State != core.SessionActive || s.InstanceHash != runnertoken.HashInstanceID("inst-1") ||
		s.StartedAt == nil || s.SandboxID != "job-1" || s.RunnerVersion != "1.0.0" {
		t.Fatalf("bad session: %+v", s)
	}
	if e.hooks.started != 1 {
		t.Fatalf("SessionStarted called %d times", e.hooks.started)
	}
}

func TestStartRepeatSameInstance(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.client().Start(context.Background(), e.boot, startReq("inst-1")); err != nil {
		t.Fatal(err)
	}
	resp, err := e.client().Start(context.Background(), e.boot, startReq("inst-1"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.SessionToken == "" {
		t.Fatal("no token")
	}
	if e.hooks.started != 1 {
		t.Fatalf("SessionStarted called %d times", e.hooks.started)
	}
}

func TestStartOtherInstance(t *testing.T) {
	e := newEnv(t, nil)
	if _, err := e.client().Start(context.Background(), e.boot, startReq("inst-1")); err != nil {
		t.Fatal(err)
	}
	_, err := e.client().Start(context.Background(), e.boot, startReq("inst-2"))
	if !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}

func TestStartUnknownBootstrap(t *testing.T) {
	e := newEnv(t, nil)
	tok, _, err := IssueBootstrap()
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.client().Start(context.Background(), tok, startReq("inst-1"))
	if !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}

func TestStartCancelledRun(t *testing.T) {
	e := newEnv(t, nil)
	e.run.State = core.RunCancelled
	if err := e.mem.UpdateRun(context.Background(), e.run); err != nil {
		t.Fatal(err)
	}
	_, err := e.client().Start(context.Background(), e.boot, startReq("inst-1"))
	if !runnerclient.IsRunCancelled(err) {
		t.Fatalf("want 410, got %v", err)
	}
}

func TestStartExpiredBootstrap(t *testing.T) {
	e := newEnv(t, nil)
	boot := e.newSessionExpired()
	_, err := e.client().Start(context.Background(), boot, startReq("inst-1"))
	if !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}

// newSessionExpired ends session 1 and makes session 2 with an expired bootstrap token.
func (e *env) newSessionExpired() string {
	e.t.Helper()
	s := e.session(1)
	s.State = core.SessionEnded
	if err := e.mem.UpdateRunSession(context.Background(), s); err != nil {
		e.t.Fatal(err)
	}
	return e.newSession(2, -time.Hour)
}

func TestStartParallelCalls(t *testing.T) {
	e := newEnv(t, nil)
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.client().Start(context.Background(), e.boot, startReq("inst-1"))
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("call %d: %v", i, err)
		}
	}
	if e.hooks.started != 1 {
		t.Fatalf("SessionStarted called %d times", e.hooks.started)
	}
}

func TestUnsupportedVersion(t *testing.T) {
	e := newEnv(t, nil)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+runnerproto.PathSessionSecrets, nil)
	req.Header.Set(runnerproto.HeaderRunnerVersion, "2.0.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body runnerproto.ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if resp.StatusCode != http.StatusUpgradeRequired || body.Code != "unsupported_runner_version" {
		t.Fatalf("got %d %+v", resp.StatusCode, body)
	}
	// A leading v and a matching major version pass the check.
	req.Header.Set(runnerproto.HeaderRunnerVersion, "v1.4.2")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 (no token), got %d", resp2.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	e := newEnv(t, nil)
	big := bytes.Repeat([]byte("a"), maxBodyBytes+1024)
	body := append([]byte(`{"instance_id":"`), big...)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+runnerproto.PathSessionStart, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.boot)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var eb runnerproto.ErrorBody
	_ = json.NewDecoder(resp.Body).Decode(&eb)
	if resp.StatusCode != http.StatusRequestEntityTooLarge || eb.Code != "too_large" {
		t.Fatalf("got %d %+v", resp.StatusCode, eb)
	}
}

func TestSecretsNoProvider(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	resp, err := c.Secrets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Env) != 0 || len(resp.Files) != 0 || len(resp.Redact) != 0 {
		t.Fatalf("want empty, got %+v", resp)
	}
}

type fakeSecrets struct{ calls atomic.Int32 }

func (f *fakeSecrets) Secrets(_ context.Context, run *core.Run, s *core.RunSession) (runnerproto.SecretsResponse, error) {
	f.calls.Add(1)
	return runnerproto.SecretsResponse{Env: map[string]string{"RUN": run.ID}}, nil
}

func TestSecretsProvider(t *testing.T) {
	fs := &fakeSecrets{}
	e := newEnv(t, func(c *Config) { c.Secrets = fs })
	c := e.started()
	for i := 0; i < 2; i++ {
		resp, err := c.Secrets(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if resp.Env["RUN"] != e.run.ID {
			t.Fatalf("bad env: %+v", resp.Env)
		}
	}
	if fs.calls.Load() != 2 {
		t.Fatalf("provider called %d times", fs.calls.Load())
	}
}

func TestTokenRefresh(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	old := c.Token()
	// Move the clock so that the new token differs from the old one.
	e.clock.advance(2 * time.Second)
	resp, err := c.RefreshToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if resp.SessionToken == "" || resp.SessionToken == old || c.Token() != resp.SessionToken {
		t.Fatal("token did not change")
	}
	if _, err := c.Secrets(context.Background()); err != nil {
		t.Fatalf("new token: %v", err)
	}
}

func TestNotImplemented(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	_, err := c.CloudToken(context.Background(), "aud", 0)
	var se *runnerclient.StatusError
	if !asStatus(err, &se) || se.Status != http.StatusNotImplemented || se.Code != "not_implemented" {
		t.Fatalf("want 501, got %v", err)
	}
}

func TestExpiredSessionToken(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	e.clock.advance(2 * time.Hour)
	_, err := c.Secrets(context.Background())
	if !runnerclient.IsUnauthorized(err) {
		t.Fatalf("want 401, got %v", err)
	}
}
