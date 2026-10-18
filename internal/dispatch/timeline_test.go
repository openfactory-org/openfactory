package dispatch

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netguard"
)

func (e *env) timelineOf(runID string) []core.TimelineEvent {
	e.t.Helper()
	list, err := e.mem.ListTimelineEvents(e.ctx, runID, time.Time{}, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	return list
}

func timelineTypes(list []core.TimelineEvent) []string {
	var out []string
	for _, ev := range list {
		out = append(out, ev.Type)
	}
	slices.Sort(out)
	return out
}

func timelineData(t *testing.T, list []core.TimelineEvent, typ string) map[string]any {
	t.Helper()
	for _, ev := range list {
		if ev.Type == typ {
			var m map[string]any
			if len(ev.Data) > 0 {
				if err := json.Unmarshal(ev.Data, &m); err != nil {
					t.Fatal(err)
				}
			}
			return m
		}
	}
	t.Fatalf("no %s event in %v", typ, timelineTypes(list))
	return nil
}

func TestTimelineHappyPath(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	s := e.session(run.ID, 1)
	e.d.SessionStarted(e.ctx, &core.RunSession{RunID: run.ID, Session: 1, TaskID: s.TaskID, RunnerVersion: "1.2.3"})
	e.makeActive(run.ID, 1)
	s = e.session(run.ID, 1)
	e.events(s, event(t, 1, runnerproto.EventTurnStarted, "t1", nil))
	e.events(s, event(t, 2, runnerproto.EventTurnEnded, "t1", runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone}))
	e.d.SessionEnded(e.ctx, s, runnerproto.EndRequest{Reason: runnerproto.EndIdle})

	list := e.timelineOf(run.ID)
	want := []string{"run.queued", "run.succeeded", "sandbox.assigned", "session.ended", "session.started"}
	if got := timelineTypes(list); !reflect.DeepEqual(got, want) {
		t.Fatalf("types = %v, want %v", got, want)
	}
	if m := timelineData(t, list, "run.queued"); m["reason"] != "new" {
		t.Errorf("queued data = %v", m)
	}
	if m := timelineData(t, list, "sandbox.assigned"); m["backend"] != "fake" {
		t.Errorf("assigned data = %v", m)
	}
	if m := timelineData(t, list, "session.started"); m["runner_version"] != "1.2.3" {
		t.Errorf("started data = %v", m)
	}
	if m := timelineData(t, list, "session.ended"); m["reason"] != "idle" {
		t.Errorf("ended data = %v", m)
	}
	for _, ev := range list {
		if ev.Type != "run.queued" && ev.Session != 1 {
			t.Errorf("%s session = %d", ev.Type, ev.Session)
		}
	}
}

func TestTimelineRetryAndError(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	for i := 1; i <= core.MaxRetrySessions; i++ {
		e.tick()
		e.makeActive(run.ID, i)
		e.advance(2 * time.Minute)
		e.backend.setStatus(core.TaskStatus{TaskID: core.TaskID(run.ID, i), State: core.TaskExited, Reason: core.SandboxEvicted})
		e.tick()
	}
	list := e.timelineOf(run.ID)
	count := map[string]int{}
	for _, ev := range list {
		count[ev.Type]++
	}
	n := core.MaxRetrySessions
	if count["sandbox.assigned"] != n || count["session.ended"] != n || count["run.retry_scheduled"] != n-1 || count["run.error"] != 1 {
		t.Fatalf("counts = %v", count)
	}
	// The first queue entry is "new", the others are "retry".
	reasons := map[string]int{}
	for _, ev := range list {
		if ev.Type == "run.queued" {
			var m map[string]any
			_ = json.Unmarshal(ev.Data, &m)
			reasons[m["reason"].(string)]++
		}
	}
	if reasons["new"] != 1 || reasons["retry"] != n-1 {
		t.Errorf("queued reasons = %v", reasons)
	}
	if m := timelineData(t, list, "run.error"); m["failure_code"] != string(core.FailureSandboxLost) {
		t.Errorf("error data = %v", m)
	}
	if m := timelineData(t, list, "run.retry_scheduled"); m["failure_code"] != string(core.FailureSandboxLost) {
		t.Errorf("retry data = %v", m)
	}
}

func TestTimelineMessageRequeue(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunSucceeded, "")
	if err := e.d.requeue(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	list := e.timelineOf(run.ID)
	if m := timelineData(t, list, "run.queued"); m["reason"] != "message" {
		t.Errorf("queued data = %v", m)
	}
}

func TestTimelineCancelIsNotWritten(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	if err := e.d.Cancel(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if got := timelineTypes(e.timelineOf(run.ID)); slices.Contains(got, "run.cancelled") {
		t.Errorf("types = %v", got)
	}
}

func TestBuildTaskRequiredHosts(t *testing.T) {
	hook := func(r *core.Run) []string {
		if r.Config.Harness.Type == core.HarnessClaude {
			return []string{"api.example-provider.test", "a.example"}
		}
		return nil
	}
	e := newEnv(t)
	d, err := New(Config{Store: e.mem, Runner: e.mem, Backend: e.backend, ControlURL: "http://control.test", RequiredHosts: hook})
	if err != nil {
		t.Fatal(err)
	}
	policy := core.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: []string{"a.example", "z.example"}}
	mk := func(h core.HarnessType, np core.EffectiveNetworkPolicy) *core.Run {
		return &core.Run{ID: "run_h", TeamID: "team", Config: core.AgentConfig{Harness: core.HarnessConfig{Type: h}, NetworkPolicy: np}}
	}
	got := d.buildTask(mk(core.HarnessClaude, policy), 1, "t").NetworkPolicy
	want := []string{"a.example", "api.example-provider.test", "z.example"}
	if got == nil || !reflect.DeepEqual(got.AllowedHosts, want) {
		t.Errorf("claude allowed = %+v, want %v", got, want)
	}
	got = d.buildTask(mk(core.HarnessType("open"), policy), 1, "t").NetworkPolicy
	if got == nil || !reflect.DeepEqual(got.AllowedHosts, []string{"a.example", "z.example"}) {
		t.Errorf("open allowed = %+v", got)
	}
	if p := d.buildTask(mk(core.HarnessClaude, core.EffectiveNetworkPolicy{}), 1, "t").NetworkPolicy; p != nil {
		t.Errorf("unrestricted run has policy %+v", p)
	}
}

// A required host in deniedHosts stays reachable: the sidecar checks the deny
// list first, so the task must not carry the host there.
func TestReviewRequiredHostCannotBeDenied(t *testing.T) {
	e := newEnv(t)
	hook := func(*core.Run) []string { return []string{"github.com"} }
	d, err := New(Config{Store: e.mem, Runner: e.mem, Backend: e.backend, ControlURL: "http://control.test", RequiredHosts: hook})
	if err != nil {
		t.Fatal(err)
	}
	run := &core.Run{ID: "run_d", TeamID: "team", Config: core.AgentConfig{NetworkPolicy: core.EffectiveNetworkPolicy{
		Restricted: true, DeniedHosts: []string{"github.com", "other.example"}}}}
	tp := d.buildTask(run, 1, "t").NetworkPolicy
	p := netguard.New(core.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: tp.AllowedHosts, DeniedHosts: tp.DeniedHosts}, nil)
	if !p.Allowed("github.com") {
		t.Errorf("required host github.com is blocked by the sidecar: %+v", tp)
	}
	if !reflect.DeepEqual(tp.DeniedHosts, []string{"other.example"}) {
		t.Errorf("denied = %v, want [other.example]", tp.DeniedHosts)
	}
}
