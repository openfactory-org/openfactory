package runner_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

type typedHarness struct{ typ rtcore.HarnessType }

func (h typedHarness) Type() rtcore.HarnessType { return h.typ }
func (h typedHarness) Run(context.Context, rtcore.Sandbox, rtcore.HarnessRequest) (<-chan rtcore.HarnessEvent, error) {
	return nil, fmt.Errorf("not used")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStateFiles(t *testing.T) {
	home := t.TempDir()
	work := filepath.Join(t.TempDir(), "my work.dir")
	enc := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(work, "-")
	claude := &runner.CoreHarness{H: typedHarness{rtcore.HarnessClaude}}
	codex := &runner.CoreHarness{H: typedHarness{rtcore.HarnessCodex}}
	other := &runner.CoreHarness{H: typedHarness{"open"}}

	for _, h := range []*runner.CoreHarness{claude, codex, other} {
		got, err := h.StateFiles(home, work)
		if err != nil || len(got) != 0 {
			t.Fatalf("missing dirs: %v %v", got, err)
		}
	}

	p := ".claude/projects/" + enc
	writeFile(t, filepath.Join(home, p, "s1.jsonl"), "{}")
	writeFile(t, filepath.Join(home, p, "sub", "s2.jsonl"), "{}")
	writeFile(t, filepath.Join(home, p, ".credentials.json"), "secret")
	writeFile(t, filepath.Join(home, p, "credentials-x.json"), "secret")
	writeFile(t, filepath.Join(home, ".claude/projects/other-dir/s3.jsonl"), "{}")
	writeFile(t, filepath.Join(home, ".claude/.credentials.json"), "secret")
	got, err := claude.StateFiles(home, work)
	sort.Strings(got)
	if want := []string{p + "/s1.jsonl", p + "/sub/s2.jsonl"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("claude: %v %v, want %v", got, err, want)
	}

	writeFile(t, filepath.Join(home, ".codex/sessions/2026/a.jsonl"), "{}")
	writeFile(t, filepath.Join(home, ".codex/sessions/auth.json"), "secret")
	writeFile(t, filepath.Join(home, ".codex/auth.json"), "secret")
	got, err = codex.StateFiles(home, work)
	if want := []string{".codex/sessions/2026/a.jsonl"}; err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("codex: %v %v, want %v", got, err, want)
	}
	if got, _ := other.StateFiles(home, work); len(got) != 0 {
		t.Fatalf("other: %v", got)
	}
}

// stateHarness is an exec harness with session files.
type stateHarness struct {
	runner.ExecHarness
	files []string
}

func (h stateHarness) StateFiles(string, string) ([]string, error) { return h.files, nil }

func newStateHarness(files ...string) stateHarness {
	return stateHarness{ExecHarness: execHarness("cat"), files: files}
}

func (s *session) savedParts() []string {
	var parts []string
	for _, ev := range s.events() {
		if ev.Type == string(runnerproto.EventSnapshotSaved) {
			parts = append(parts, decode[runnerproto.SnapshotSavedData](s.e.t, ev).Part)
		}
	}
	return parts
}

func TestHarnessStateSavedAfterTurn(t *testing.T) {
	e := newSnapEnv(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".fake/session.jsonl"), "hello")
	s1 := e.newSession(1, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Snapshot: runnerproto.SnapshotConfig{Enabled: true}}}, newStateHarness(".fake/session.jsonl"))
	s1.cfg.HomeDir = home
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "go")
	waitFor(t, "snapshot.saved", func() bool { return s1.has(runnerproto.EventSnapshotSaved) })
	ev, _ := s1.find(runnerproto.EventSnapshotSaved, nil)
	saved := decode[runnerproto.SnapshotSavedData](t, ev)
	if saved.Part != "harness_state" || len(saved.UploadIDs) != 2 || saved.Bytes <= 0 {
		t.Fatalf("snapshot.saved %+v", saved)
	}
	found := false
	for k := range e.cp.Objects() {
		found = found || strings.HasSuffix(k, "/harness_state/harness/state.tar.zst")
	}
	if !found {
		t.Fatalf("no state object in %v", e.cp.Objects())
	}
	s1.stop()
}

func TestHarnessStateRestore(t *testing.T) {
	e := newSnapEnv(t)
	home1 := t.TempDir()
	writeFile(t, filepath.Join(home1, ".fake/session.jsonl"), "hello")
	snap := runnerproto.SnapshotConfig{Enabled: true}
	s1 := e.newSession(1, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Snapshot: snap}}, newStateHarness(".fake/session.jsonl"))
	s1.cfg.HomeDir = home1
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "go")
	waitFor(t, "snapshot.saved", func() bool { return len(s1.savedParts()) > 0 })
	s1.stop()

	home2 := t.TempDir()
	snap.Restore = true
	s2 := e.newSession(2, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Snapshot: snap}}, newStateHarness())
	s2.cfg.HomeDir = home2
	s2.run()
	s2.waitReady()
	ev, ok := s2.find(runnerproto.EventSnapshotRestored, nil)
	if !ok {
		t.Fatal("no snapshot.restored")
	}
	got := decode[runnerproto.SnapshotRestoredData](t, ev)
	if !reflect.DeepEqual(got.Parts, []string{"workspace", "harness_state"}) && !reflect.DeepEqual(got.Parts, []string{"harness_state"}) {
		t.Fatalf("restored %+v", got)
	}
	if len(got.FailedPaths) != 0 {
		t.Fatalf("failed paths %v", got.FailedPaths)
	}
	if b, err := os.ReadFile(filepath.Join(home2, ".fake/session.jsonl")); err != nil || string(b) != "hello" {
		t.Fatalf("session file: %q %v", b, err)
	}
	s2.stop()
}

func TestMaxPartBytes(t *testing.T) {
	e := newSnapEnv(t)
	home := t.TempDir()
	blob := make([]byte, 4096)
	_, _ = rand.Read(blob)
	writeFile(t, filepath.Join(home, ".fake/session.jsonl"), string(blob))
	snap := runnerproto.SnapshotConfig{Enabled: true, MaxPartBytes: 100}
	s1 := e.newSession(1, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Snapshot: snap}}, newStateHarness(".fake/session.jsonl"))
	s1.cfg.HomeDir = home
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "go")
	waitFor(t, "too_large diagnostic", func() bool { return s1.diagnostic("snapshot/too_large") })
	s1.stop()
	if got := s1.savedParts(); len(got) != 0 {
		t.Fatalf("saved parts %v", got)
	}
	if e.objectCount() != 0 {
		t.Fatalf("%d objects uploaded", e.objectCount())
	}
	if !s1.diagnostic("snapshot/too_large") {
		t.Fatal("no diagnostic")
	}
	s1.wantEnded("stopped")
}

func TestSkipWorkspace(t *testing.T) {
	e := newSnapEnv(t)
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".fake/session.jsonl"), "x")
	snap := runnerproto.SnapshotConfig{Enabled: true, SkipWorkspace: true}
	s1 := e.newSession(1, repoStart(makeRepo(t), snap, nil), newStateHarness(".fake/session.jsonl"))
	s1.cfg.HomeDir = home
	s1.run()
	s1.waitReady()
	s1.runTurn("t1", "go")
	waitFor(t, "snapshot.saved", func() bool { return len(s1.savedParts()) > 0 })
	s1.stop()
	if got := s1.savedParts(); !reflect.DeepEqual(got, []string{"harness_state"}) {
		t.Fatalf("saved parts %v", got)
	}
	for k := range e.cp.Objects() {
		if strings.Contains(k, "/workspace/") {
			t.Fatalf("workspace object %s", k)
		}
	}
}

func TestDeclarations(t *testing.T) {
	cases := []string{"plain", "script"}
	for _, kind := range cases {
		t.Run(kind, func(t *testing.T) {
			if kind == "script" && runtime.GOOS == "windows" {
				t.Skip("no shell script on Windows")
			}
			e := newSnapEnv(t)
			extra := filepath.Join(t.TempDir(), "tool", "state.json")
			writeFile(t, extra, "state-1")
			if err := os.Chmod(extra, 0o640); err != nil && runtime.GOOS != "windows" {
				t.Fatal(err)
			}
			decl := filepath.Join(t.TempDir(), "decl")
			line := fmt.Sprintf(`{"kind":"file","path":%q}`, extra)
			missing := `{"kind":"file","path":"nothing.txt"}`
			if kind == "plain" {
				writeFile(t, decl, line+"\nnot json\n"+missing+"\n")
			} else {
				writeFile(t, decl, "#!/bin/sh\ncat <<'EOF'\n"+line+"\nnot json\n"+missing+"\nEOF\n")
				if err := os.Chmod(decl, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			snap := runnerproto.SnapshotConfig{Enabled: true}
			s1 := e.newSession(1, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Snapshot: snap}}, execHarness("cat"))
			s1.cfg.SnapshotDeclarations = decl
			s1.run()
			s1.waitReady()
			s1.stop()
			if !s1.diagnostic("snapshot/bad_declaration") {
				t.Fatal("no bad_declaration diagnostic")
			}
			var saved []byte
			for k, v := range e.cp.Objects() {
				if strings.HasSuffix(k, "/workspace/files/0") {
					saved = v
				}
			}
			if string(saved) != "state-1" {
				t.Fatalf("declared file object: %q", saved)
			}

			if err := os.Remove(extra); err != nil {
				t.Fatal(err)
			}
			snap.Restore = true
			s2 := e.newSession(2, runnerproto.StartResponse{Workspace: runnerproto.Workspace{Snapshot: snap}}, execHarness("cat"))
			s2.run()
			s2.waitReady()
			ev, _ := s2.find(runnerproto.EventSnapshotRestored, nil)
			if got := decode[runnerproto.SnapshotRestoredData](t, ev); len(got.FailedPaths) != 0 {
				t.Fatalf("failed paths %v", got.FailedPaths)
			}
			if b, err := os.ReadFile(extra); err != nil || string(b) != "state-1" {
				t.Fatalf("restored file: %q %v", b, err)
			}
			if runtime.GOOS != "windows" {
				if fi, err := os.Stat(extra); err != nil || fi.Mode().Perm() != 0o640 {
					t.Fatalf("mode %v %v", fi, err)
				}
			}
			s2.stop()
		})
	}
}
