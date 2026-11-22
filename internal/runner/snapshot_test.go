package runner_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runner/runnertest"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

type fakeGitCreds struct {
	forbid atomic.Bool
	mu     sync.Mutex
	got    runnerproto.GitCredentialRequest
}

func (f *fakeGitCreds) answer(req runnerproto.GitCredentialRequest) (runnerproto.GitCredentialResponse, int) {
	f.mu.Lock()
	f.got = req
	f.mu.Unlock()
	if f.forbid.Load() {
		return runnerproto.GitCredentialResponse{}, http.StatusForbidden
	}
	return runnerproto.GitCredentialResponse{Username: "x-access-token", Password: "tok-123"}, 0
}

// snapEnv is a control plane for runs with more than one session.
type snapEnv struct {
	t     *testing.T
	cp    *runnertest.Server
	run   testRun
	creds *fakeGitCreds
}

func newSnapEnv(t *testing.T) *snapEnv {
	t.Helper()
	gitIdentity(t)
	e := &snapEnv{t: t, cp: runnertest.New(t, runnertest.Options{}), creds: &fakeGitCreds{}}
	e.cp.GitCredential = e.creds.answer
	e.run = testRun{ID: e.cp.RunID()}
	return e
}

// objectCount returns the number of stored objects.
func (e *snapEnv) objectCount() int { return len(e.cp.Objects()) }

func gitIdentity(t *testing.T) {
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "tester")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "tester@example.test")
	}
}

// session is a runner of one session of the run.
type session struct {
	e     *snapEnv
	n     int
	cfg   runner.Config
	work  string
	state string
	done  <-chan error
}

// newSession adds session n with the start response resp and returns its runner configuration.
func (e *snapEnv) newSession(n int, resp runnerproto.StartResponse, h runner.Harness) *session {
	e.t.Helper()
	if resp.Harness.Type == "" {
		resp.Harness.Type = "exec"
	}
	tok := e.cp.AddSession(n, resp)
	se := &session{e: e, n: n, work: filepath.Join(e.t.TempDir(), "work"), state: e.t.TempDir()}
	se.cfg = runner.Config{
		ControlURL: e.cp.URL(), RunID: e.run.ID, Session: n, BootstrapToken: tok,
		WorkDir: se.work, StateDir: se.state,
		Harnesses: map[string]runner.Harness{"exec": h}, Version: "1.0.0", SandboxID: "test",
		Logger:   testLogger(),
		PollWait: 1,
	}
	return se
}

func (s *session) run() {
	ch := make(chan error, 1)
	go func() { ch <- runner.Run(context.Background(), s.cfg) }()
	s.done = ch
}

func (s *session) events() []runnertest.Event { return s.e.cp.Events(s.n) }

func (s *session) find(typ runnerproto.EventType, pred func(runnertest.Event) bool) (runnertest.Event, bool) {
	for _, ev := range s.events() {
		if ev.Type == string(typ) && (pred == nil || pred(ev)) {
			return ev, true
		}
	}
	return runnertest.Event{}, false
}

func (s *session) has(typ runnerproto.EventType) bool {
	_, ok := s.find(typ, nil)
	return ok
}

func (s *session) waitReady() {
	s.e.t.Helper()
	waitFor(s.e.t, "ready", func() bool { return s.has(runnerproto.EventSessionReady) })
}

func (s *session) command(typ runnerproto.CommandType, data any) {
	s.e.t.Helper()
	s.e.cp.Enqueue(s.n, typ, data)
}

func (s *session) startTurn(id, content string) {
	s.e.t.Helper()
	s.command(runnerproto.CommandTurnStart, runnerproto.TurnStartData{
		TurnID: id,
		Input: []runnerproto.InputItem{{
			MessageID: "msg_1", Kind: runnerproto.KindUser,
			Sender: runnerproto.Sender{Type: "user", Name: "Dana"}, Content: content,
			Attachments: []runnerproto.Attachment{},
		}},
	})
}

// runTurn starts a turn and waits for its end.
func (s *session) runTurn(id, content string) {
	s.startTurn(id, content)
	waitFor(s.e.t, "turn.ended", func() bool { return s.has(runnerproto.EventTurnEnded) })
}

// stop stops the session and waits for the result of Run.
func (s *session) stop() {
	s.e.t.Helper()
	s.command(runnerproto.CommandSessionStop, runnerproto.SessionStopData{Reason: "shutdown"})
	if err := waitResult(s.e.t, s.done, 20*time.Second); err != nil {
		s.e.t.Fatal(err)
	}
}

func (s *session) wantEnded(reason string) {
	s.e.t.Helper()
	got := s.e.cp.Session(s.n)
	if got.State != runnertest.SessionEnded || got.EndReason != reason {
		s.e.t.Fatalf("session state %q reason %q, want ended %q", got.State, got.EndReason, reason)
	}
}

func (s *session) message() string {
	s.e.t.Helper()
	ev, ok := s.find(runnerproto.EventMessage, nil)
	if !ok {
		s.e.t.Fatal("no message event")
	}
	return decode[runnerproto.MessageData](s.e.t, ev).Content
}

func (s *session) diagnostic(code string) bool {
	_, ok := s.find(runnerproto.EventDiagnostic, func(ev runnertest.Event) bool {
		return decode[runnerproto.DiagnosticData](s.e.t, ev).Code == code
	})
	return ok
}

// makeRepo makes a git repository with the file a.txt and returns its path.
func makeRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "origin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("orig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"commit", "-q", "-m", "first"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func repoStart(origin string, snap runnerproto.SnapshotConfig, env map[string]string) runnerproto.StartResponse {
	return runnerproto.StartResponse{
		Env: env,
		Workspace: runnerproto.Workspace{
			Repositories: []runnerproto.Repository{{CloneURL: origin, Path: "repo"}},
			Snapshot:     snap,
		},
	}
}

const changeRepo = "cd repo && echo changed > a.txt && echo new > untracked.txt && cat >/dev/null"

func TestSnapshotRoundTrip(t *testing.T) {
	e := newSnapEnv(t)
	origin := makeRepo(t)

	s1 := e.newSession(1, repoStart(origin, runnerproto.SnapshotConfig{Enabled: true}, nil), execHarness(changeRepo))
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "go")
	if d, _ := s1.find(runnerproto.EventTurnEnded, nil); decode[runnerproto.TurnEndedData](t, d).Outcome != runnerproto.OutcomeDone {
		t.Fatalf("turn did not end with done: %s", d.Data)
	}
	s1.stop()
	s1.wantEnded("stopped")
	ev, ok := s1.find(runnerproto.EventSnapshotSaved, nil)
	if !ok {
		t.Fatal("no snapshot.saved event")
	}
	saved := decode[runnerproto.SnapshotSavedData](t, ev)
	if saved.Part != "workspace" || len(saved.UploadIDs) < 2 || saved.Bytes <= 0 {
		t.Fatalf("snapshot.saved %+v", saved)
	}

	s2 := e.newSession(2, repoStart(origin, runnerproto.SnapshotConfig{Restore: true, Enabled: true}, nil), execHarness("cat"))
	s2.run()
	s2.waitReady()
	ev, ok = s2.find(runnerproto.EventSnapshotRestored, nil)
	if !ok {
		t.Fatal("no snapshot.restored event")
	}
	got := decode[runnerproto.SnapshotRestoredData](t, ev)
	if len(got.Parts) != 1 || got.Parts[0] != "workspace" || len(got.FailedPaths) != 0 {
		t.Fatalf("snapshot.restored %+v", got)
	}
	if b, err := os.ReadFile(filepath.Join(s2.work, "repo", "a.txt")); err != nil || strings.TrimSpace(string(b)) != "changed" {
		t.Fatalf("a.txt: %q %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(s2.work, "repo", "untracked.txt")); err != nil || strings.TrimSpace(string(b)) != "new" {
		t.Fatalf("untracked.txt: %q %v", b, err)
	}
	s2.runTurn("t2", "hello")
	msg := s2.message()
	if !strings.HasPrefix(msg, "[system from platform]\nThe workspace was restored from the previous session.") || !strings.HasSuffix(msg, "hello") {
		t.Fatalf("first message %q", msg)
	}
	s2.stop()
}

func TestSnapshotRestoreWithoutSnapshot(t *testing.T) {
	e := newSnapEnv(t)
	origin := makeRepo(t)
	s1 := e.newSession(1, repoStart(origin, runnerproto.SnapshotConfig{}, nil), execHarness("cat"))
	s1.run()
	s1.waitReady()
	s1.stop()
	if s1.has(runnerproto.EventSnapshotSaved) {
		t.Fatal("snapshot.saved without Enabled")
	}

	s2 := e.newSession(2, repoStart(origin, runnerproto.SnapshotConfig{Restore: true}, nil), execHarness("cat"))
	s2.run()
	s2.waitReady()
	ev, ok := s2.find(runnerproto.EventSnapshotRestored, nil)
	if !ok {
		t.Fatal("no snapshot.restored event")
	}
	if got := decode[runnerproto.SnapshotRestoredData](t, ev); len(got.Parts) != 0 || len(got.FailedPaths) != 0 {
		t.Fatalf("snapshot.restored %+v", got)
	}
	if !strings.Contains(string(ev.Data), `"parts":[]`) {
		t.Fatalf("parts must be an empty list: %s", ev.Data)
	}
	s2.runTurn("t1", "hello")
	if msg := s2.message(); !strings.Contains(msg, "The workspace is new.") {
		t.Fatalf("first message %q", msg)
	}
	s2.stop()
}

func TestNoRestoreNoteInSessionOne(t *testing.T) {
	e := newSnapEnv(t)
	s1 := e.newSession(1, repoStart(makeRepo(t), runnerproto.SnapshotConfig{Restore: true}, nil), execHarness("cat"))
	s1.run()
	s1.waitReady()
	if s1.has(runnerproto.EventSnapshotRestored) {
		t.Fatal("snapshot.restored in session 1")
	}
	s1.runTurn("t1", "hello")
	if msg := s1.message(); msg != "hello" {
		t.Fatalf("message %q", msg)
	}
	s1.stop()
}

func TestSnapshotSkippedAfterSetupFailure(t *testing.T) {
	e := newSnapEnv(t)
	resp := repoStart(makeRepo(t), runnerproto.SnapshotConfig{Enabled: true}, nil)
	resp.Workspace.SetupCommands = []runnerproto.SetupCommand{{Command: "exit 1"}}
	s1 := e.newSession(1, resp, execHarness("cat"))
	s1.run()
	if err := waitResult(t, s1.done, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	s1.wantEnded("setup_failed")
	if s1.has(runnerproto.EventSnapshotSaved) || e.objectCount() != 0 {
		t.Fatal("a snapshot was taken after a setup failure")
	}
}

func TestSnapshotUploadError(t *testing.T) {
	e := newSnapEnv(t)
	e.cp.SetPutStatus(http.StatusInternalServerError)
	s1 := e.newSession(1, repoStart(makeRepo(t), runnerproto.SnapshotConfig{Enabled: true}, nil), execHarness(changeRepo))
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "go")
	s1.stop()
	s1.wantEnded("stopped")
	ev, ok := s1.find(runnerproto.EventDiagnostic, func(ev runnertest.Event) bool {
		return decode[runnerproto.DiagnosticData](t, ev).Code == "snapshot_failed"
	})
	if !ok {
		t.Fatal("no snapshot_failed diagnostic")
	}
	if strings.Contains(string(ev.Data), e.cp.URL()) {
		t.Fatalf("the diagnostic has the upload URL: %s", ev.Data)
	}
	if s1.has(runnerproto.EventSnapshotSaved) {
		t.Fatal("snapshot.saved after an upload error")
	}
}

func TestGitCredentialHelper(t *testing.T) {
	e := newSnapEnv(t)
	s1 := e.newSession(1, runnerproto.StartResponse{}, execHarness("cat"))
	s1.run()
	s1.waitReady()
	defer s1.stop()

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(filepath.Join(s1.state, "session.json"))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("session.json: %v %v", fi, err)
		}
	}
	in := "protocol=https\nhost=example.test\npath=acme/api.git\n\n"
	var buf bytes.Buffer
	if err := runner.GitCredential(context.Background(), s1.state, "get", strings.NewReader(in), &buf); err != nil {
		t.Fatal(err)
	}
	if want := "username=x-access-token\npassword=tok-123\n"; buf.String() != want {
		t.Fatalf("output %q, want %q", buf.String(), want)
	}
	e.creds.mu.Lock()
	got := e.creds.got
	e.creds.mu.Unlock()
	if got.Protocol != "https" || got.Host != "example.test" || got.Path != "acme/api.git" {
		t.Fatalf("request %+v", got)
	}

	for _, op := range []string{"store", "erase"} {
		buf.Reset()
		if err := runner.GitCredential(context.Background(), s1.state, op, strings.NewReader(in), &buf); err != nil || buf.Len() != 0 {
			t.Fatalf("%s: %v %q", op, err, buf.String())
		}
	}

	e.creds.forbid.Store(true)
	buf.Reset()
	err := runner.GitCredential(context.Background(), s1.state, "get", strings.NewReader(in), &buf)
	if err == nil || buf.Len() != 0 || strings.Contains(err.Error(), "tok-123") {
		t.Fatalf("forbidden: err %v output %q", err, buf.String())
	}
	if err := runner.GitCredential(context.Background(), t.TempDir(), "get", strings.NewReader(in), &buf); err == nil {
		t.Fatal("no error without a session file")
	}
}

func TestCloneCredentialConfig(t *testing.T) {
	e := newSnapEnv(t)
	s1 := e.newSession(1, repoStart(makeRepo(t), runnerproto.SnapshotConfig{}, nil), execHarness("cat"))
	s1.cfg.GitCredentialHelper = "!true"
	s1.run()
	s1.waitReady()
	defer s1.stop()
	for key, want := range map[string]string{"credential.helper": "!true", "credential.useHttpPath": "true"} {
		out, err := exec.Command("git", "-C", filepath.Join(s1.work, "repo"), "config", "--local", key).Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Fatalf("%s: %q %v, want %q", key, out, err, want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(s1.work, "repo", ".git", "info", "exclude")); err != nil || !strings.Contains(string(b), ".openfactory/") {
		t.Fatalf("exclude file: %q %v", b, err)
	}
}

func TestWorkDirFromEnv(t *testing.T) {
	cases := []struct{ name, value, suffix string }{
		{"relative", "repo", "/repo"},
		{"bad", "../escape", "/work"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newSnapEnv(t)
			resp := runnerproto.StartResponse{Env: map[string]string{"OF_WORKDIR": c.value}}
			resp.Workspace.SetupCommands = []runnerproto.SetupCommand{{Command: "mkdir -p repo"}}
			s1 := e.newSession(1, resp, execHarness("pwd"))
			s1.run()
			s1.waitReady()
			s1.runTurn("t1", "go")
			if msg := filepath.ToSlash(s1.message()); !strings.HasSuffix(msg, c.suffix) {
				t.Fatalf("pwd %q, want suffix %q", msg, c.suffix)
			}
			if got := s1.diagnostic("bad_workdir"); got != (c.name == "bad") {
				t.Fatalf("bad_workdir diagnostic: %v", got)
			}
			s1.stop()
		})
	}
}
