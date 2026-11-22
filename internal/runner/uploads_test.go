package runner_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runner"
	"github.com/openfactory-org/openfactory/internal/runner/runnertest"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func (s *session) events2(typ runnerproto.EventType) []runnertest.Event {
	var out []runnertest.Event
	for _, ev := range s.events() {
		if ev.Type == string(typ) {
			out = append(out, ev)
		}
	}
	return out
}

func (e *snapEnv) object(key string) (string, bool) {
	b, ok := e.cp.Object(key)
	return string(b), ok
}

const writeArtifact = "mkdir -p .openfactory/artifacts/out && echo hello > .openfactory/artifacts/out/report.txt && cat >/dev/null"

func TestArtifactUploadAtTurnEnd(t *testing.T) {
	e := newSnapEnv(t)
	s := e.newSession(1, runnerproto.StartResponse{}, execHarness(writeArtifact))
	s.run()
	s.waitReady()
	s.runTurn("t1", "go")
	evs := s.events2(runnerproto.EventArtifact)
	if len(evs) != 1 {
		t.Fatalf("artifact events = %d", len(evs))
	}
	d := decode[runnerproto.ArtifactData](t, evs[0])
	if d.Name != "out/report.txt" || d.Type != "FILE" || d.UploadID == "" {
		t.Fatalf("artifact event = %+v", d)
	}
	key, ok := e.cp.ObjectKey(d.UploadID)
	if !ok {
		t.Fatal("the upload ID does not resolve")
	}
	if want := "runs/" + e.run.ID + "/sessions/1/artifact/out/report.txt"; key != want {
		t.Errorf("key = %q, want %q", key, want)
	}
	if body, ok := e.object(key); !ok || body != "hello\n" {
		t.Errorf("object = %q, %v", body, ok)
	}
	s.stop()
}

// The artifact limit is 100 files for a session. The 101st gets the
// diagnostic artifact/limit.
func TestArtifactFileLimit(t *testing.T) {
	e := newSnapEnv(t)
	script := `mkdir -p .openfactory/artifacts && for i in $(seq 1 101); do echo $i > .openfactory/artifacts/f$i.txt; done; cat >/dev/null`
	s := e.newSession(1, runnerproto.StartResponse{}, execHarness(script))
	s.run()
	s.waitReady()
	s.runTurn("t1", "go")
	if n := len(s.events2(runnerproto.EventArtifact)); n != 100 {
		t.Errorf("artifact events = %d, want 100", n)
	}
	if !s.diagnostic("artifact/limit") {
		t.Error("no artifact/limit diagnostic")
	}
	s.stop()
}

// A turn that changes nothing sends no new event. A changed file is sent again.
func TestArtifactOnlyChanged(t *testing.T) {
	e := newSnapEnv(t)
	s := e.newSession(1, runnerproto.StartResponse{}, execHarness("cat >/dev/null; if [ -f .openfactory/artifacts/a.txt ]; then if [ -f again ]; then echo more >> .openfactory/artifacts/a.txt; fi; touch again; else mkdir -p .openfactory/artifacts && echo one > .openfactory/artifacts/a.txt; fi"))
	s.run()
	s.waitReady()
	count := func() int { return len(s.events2(runnerproto.EventArtifact)) }
	s.runTurn("t1", "go")
	if count() != 1 {
		t.Fatalf("after turn 1: %d events", count())
	}
	s.startTurn("t2", "go")
	waitFor(t, "second turn.ended", func() bool { return len(s.events2(runnerproto.EventTurnEnded)) == 2 })
	if count() != 1 {
		t.Fatalf("an unchanged file was sent again: %d events", count())
	}
	s.startTurn("t3", "go")
	waitFor(t, "third turn.ended", func() bool { return len(s.events2(runnerproto.EventTurnEnded)) == 3 })
	if count() != 2 {
		t.Fatalf("a changed file was not sent again: %d events", count())
	}
	s.stop()
}

// transcriptHarness is an exec harness with a raw transcript file.
type transcriptHarness struct {
	runner.ExecHarness
	path string
}

func (h transcriptHarness) TranscriptPath(_, _ string) string { return h.path }

func TestTranscriptUploadIsRedacted(t *testing.T) {
	const secret = "s3cr3t-value-123"
	e := newSnapEnv(t)
	e.cp.SetSecrets(runnerproto.SecretsResponse{Redact: []string{secret}})
	home := t.TempDir()
	tpath := filepath.Join(home, "session.jsonl")
	h := transcriptHarness{ExecHarness: execHarness("cat >/dev/null"), path: tpath}
	if err := os.WriteFile(tpath, []byte(`{"text":"key `+secret+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := e.newSession(1, runnerproto.StartResponse{}, h)
	s.cfg.HomeDir = home
	s.run()
	s.waitReady()
	s.runTurn("t1", "go")
	evs := s.events2(runnerproto.EventTranscriptSaved)
	if len(evs) != 1 {
		t.Fatalf("transcript.saved events = %d", len(evs))
	}
	d := decode[runnerproto.TranscriptSavedData](t, evs[0])
	key, ok := e.cp.ObjectKey(d.UploadID)
	if !ok {
		t.Fatal("the upload ID does not resolve")
	}
	body, ok := e.object(key)
	if !ok {
		t.Fatalf("no object %s", key)
	}
	if strings.Contains(body, secret) || !strings.Contains(body, `"key ***"`) {
		t.Errorf("transcript = %q", body)
	}
	s.stop()
}

func TestCoreHarnessTranscriptPath(t *testing.T) {
	home, work := t.TempDir(), t.TempDir()
	enc := strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, work)
	dir := filepath.Join(home, ".claude", "projects", enc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess-1.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &runner.CoreHarness{H: typedHarness{rtcore.HarnessClaude}}
	if got := h.TranscriptPath(home, work); got != filepath.Join(dir, "sess-1.jsonl") {
		t.Errorf("claude: got %q", got)
	}
	if got := (&runner.CoreHarness{H: typedHarness{rtcore.HarnessCodex}}).TranscriptPath(home, work); got != "" {
		t.Errorf("codex without files: got %q", got)
	}
	if got := (&runner.CoreHarness{H: typedHarness{rtcore.HarnessType("open")}}).TranscriptPath(home, work); got != "" {
		t.Errorf("open: got %q", got)
	}
}
