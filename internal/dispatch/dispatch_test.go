package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
)

func TestNewChecksConfig(t *testing.T) {
	e := newEnv(t)
	good := Config{Store: e.mem, Runner: e.mem, Backend: e.backend, ControlURL: "http://x"}
	if _, err := New(good); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*Config){
		"store":   func(c *Config) { c.Store = nil },
		"runner":  func(c *Config) { c.Runner = nil },
		"backend": func(c *Config) { c.Backend = nil },
		"url":     func(c *Config) { c.ControlURL = "" },
	} {
		c := good
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestTickClaimsQueuedRun(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "do it")
	e.tick()

	s := e.session(run.ID, 1)
	if s.State != core.SessionAssigned || s.BootstrapHash == "" || s.TaskID != run.ID+".1" || s.Backend != "fake" {
		t.Fatalf("session = %+v", s)
	}
	if got := e.run(run.ID).State; got != core.RunPending {
		t.Fatalf("run state = %s", got)
	}
	tasks := e.backend.startedTasks()
	if len(tasks) != 1 {
		t.Fatalf("started %d tasks", len(tasks))
	}
	env := tasks[0].Env
	if env[core.TaskEnvControlURL] != "http://control.test" || env[core.TaskEnvRunID] != run.ID || env[core.TaskEnvSession] != "1" {
		t.Errorf("env = %v", env)
	}
	hash, err := runnertoken.HashBootstrap(env[core.TaskEnvBootstrapToken])
	if err != nil || hash != s.BootstrapHash {
		t.Errorf("token hash = %q, %v; stored %q", hash, err, s.BootstrapHash)
	}
	if tasks[0].TeamID != "team" || tasks[0].FactoryID != "fac" || tasks[0].Image != "img" {
		t.Errorf("task = %+v", tasks[0].Redacted())
	}
	if in := e.inputs(run.ID); len(in) != 1 || in[0].Content != "do it" || in[0].Sender.Type != "platform" {
		t.Errorf("inputs = %+v", in)
	}
}

func TestTickTwiceNoSecondSession(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	e.tick()
	if list, _ := e.mem.ListRunSessions(e.ctx, run.ID); len(list) != 1 {
		t.Errorf("sessions = %d", len(list))
	}
	if in := e.inputs(run.ID); len(in) != 1 {
		t.Errorf("inputs = %d", len(in))
	}
	if len(e.backend.startedTasks()) != 1 {
		t.Error("task started twice")
	}
}

func TestCapacity(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.backend.startErr = core.ErrAtCapacity
	e.tick() // must not fail
	s := e.session(run.ID, 1)
	if s.State != core.SessionEnded || s.EndReason != "not_started" {
		t.Fatalf("session = %+v", s)
	}
	// runstate: resource_unavailable is a retry code, so the run is queued again.
	if got := e.run(run.ID).State; got != core.RunQueued {
		t.Fatalf("run state = %s", got)
	}
	e.tick()
	if _, err := e.mem.GetRunSession(e.ctx, run.ID, 2); err != nil {
		t.Fatalf("session 2: %v", err)
	}
	if in := e.inputs(run.ID); len(in) != 1 {
		t.Errorf("prompt count = %d", len(in))
	}
}

func TestStartErrorGivesUp(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.backend.startErr = errors.New("boom")
	for i := 1; i <= core.MaxRetrySessions; i++ {
		e.tick()
		want := core.RunQueued
		if i == core.MaxRetrySessions {
			want = core.RunError // platform failure code, no retries left
		}
		if got := e.run(run.ID).State; got != want {
			t.Fatalf("after attempt %d: state = %s, want %s", i, got, want)
		}
	}
	r := e.run(run.ID)
	if r.Error != string(core.FailureSandboxCreateFailed) || r.FinishedAt == nil {
		t.Errorf("run = %+v", r)
	}
	e.tick()
	if list, _ := e.mem.ListRunSessions(e.ctx, run.ID); len(list) != core.MaxRetrySessions {
		t.Errorf("sessions = %d", len(list))
	}
}

func TestSessionStartedClaims(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
	if got := e.run(run.ID).State; got != core.RunClaimed {
		t.Fatalf("state = %s", got)
	}
	// A second call is refused by runstate and changes nothing.
	e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
	if got := e.run(run.ID).State; got != core.RunClaimed {
		t.Fatalf("state = %s", got)
	}
}

func TestDelivery(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "the prompt")
	e.tick()
	e.tick() // session is not active: nothing is sent
	if len(e.commands(run.ID, 1)) != 0 {
		t.Fatal("command before ready")
	}
	e.makeActive(run.ID, 1)
	e.tick()
	cmds := e.commands(run.ID, 1)
	if len(cmds) != 1 || cmds[0].Type != string(runnerproto.CommandTurnStart) {
		t.Fatalf("commands = %+v", cmds)
	}
	var data runnerproto.TurnStartData
	if err := json.Unmarshal(cmds[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Input) != 1 || data.Input[0].Content != "the prompt" || data.TurnID == "" {
		t.Fatalf("data = %+v", data)
	}
	in := e.inputs(run.ID, core.RunInputDelivered)
	if len(in) != 1 || in[0].TurnID != data.TurnID || in[0].Session != 1 {
		t.Fatalf("delivered = %+v", in)
	}
	if got := e.session(run.ID, 1).ActiveTurnID; got != data.TurnID {
		t.Errorf("active turn = %q", got)
	}
	e.tick()
	if len(e.commands(run.ID, 1)) != 1 {
		t.Error("second command")
	}
}

// liveRun makes a run with an active session and one delivered turn.
func liveRun(t *testing.T, e *env) (*core.Run, string) {
	t.Helper()
	run := e.newRun(core.RunQueued, "start")
	e.tick()
	e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
	e.makeActive(run.ID, 1)
	e.tick()
	return run, e.session(run.ID, 1).ActiveTurnID
}

func TestEventsStored(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)

	e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil))
	r := e.run(run.ID)
	if r.State != core.RunRunning || r.StartedAt == nil {
		t.Fatalf("run = %+v", r)
	}
	e.events(s,
		event(t, 2, runnerproto.EventMessage, turn, runnerproto.MessageData{Role: "assistant", Content: "hello"}),
		event(t, 3, runnerproto.EventToolCall, turn, runnerproto.ToolCallData{CallID: "c1", Name: "bash"}),
		event(t, 4, runnerproto.EventToolResult, turn, runnerproto.ToolResultData{CallID: "c1", Content: "out"}),
		event(t, 5, runnerproto.EventHarnessSession, turn, runnerproto.HarnessSessionData{SessionID: "hs-1"}),
		event(t, 6, runnerproto.EventUsage, turn, runnerproto.UsageData{InputTokens: 10, OutputTokens: 5, CostUSD: 0.5, Cumulative: false}),
	)
	if got := e.run(run.ID).Usage.InputTokens; got != 0 {
		t.Errorf("non-cumulative usage applied: %d", got)
	}
	e.events(s, event(t, 7, runnerproto.EventUsage, turn, runnerproto.UsageData{InputTokens: 10, OutputTokens: 5, CostUSD: 0.5, Cumulative: true}))
	msgs, err := e.mem.ListRunMessages(e.ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	roles := ""
	for _, m := range msgs {
		roles += m.Role + ":" + m.Content + ";"
	}
	if roles != "assistant:hello;tool_call:bash;tool_result:out;" {
		t.Errorf("messages = %s", roles)
	}
	r = e.run(run.ID)
	if r.SessionID != "hs-1" || r.Usage.InputTokens != 10 || r.Usage.OutputTokens != 5 || r.Usage.CostUSD != 0.5 {
		t.Errorf("run = %+v", r)
	}
	e.events(s, event(t, 8, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "ok"}))
	r = e.run(run.ID)
	if r.State != core.RunSucceeded || r.Result != "ok" {
		t.Errorf("run = %+v", r)
	}
}

func TestTurnEndedError(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)
	e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil),
		event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeError, FailureCode: "harness_auth_failed"}))
	r := e.run(run.ID)
	if r.State != core.RunFailed || r.Error != "harness_auth_failed" {
		t.Errorf("run = %+v", r)
	}
}

// The text of a failed turn goes to the error of the run, after the code.
func TestTurnEndedErrorMessage(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)
	msg := "claude exited with status 1: Invalid API key"
	e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil),
		event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeError, FailureCode: "agent_failed", Message: msg}))
	r := e.run(run.ID)
	if r.State != core.RunFailed || r.FailureCode != core.FailureAgentFailed || r.Error != "agent_failed: "+msg {
		t.Errorf("run = %+v", r)
	}
}

func TestFollowUpWhileLive(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	s := e.session(run.ID, 1)
	e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil),
		event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone, Result: "ok"}))
	// The service accepts the messages at turn start, then finishes the turn
	// and clears the active turn.
	if _, err := e.mem.MarkRunInputsAcceptedByTurn(e.ctx, run.ID, turn); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.mem.FinishRunInputsOfTurn(e.ctx, run.ID, turn); err != nil {
		t.Fatal(err)
	}
	e.updateSession(run.ID, 1, func(s *core.RunSession) { s.ActiveTurnID = "" })

	out, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "user", Content: "again", Sender: core.RunInputSender{Type: "user", ID: "u1"}})
	if err != nil || out != OutcomeLiveSession {
		t.Fatalf("send = %q, %v", out, err)
	}
	cmds := e.commands(run.ID, 1)
	if len(cmds) != 2 || cmds[1].Type != string(runnerproto.CommandTurnStart) {
		t.Fatalf("commands = %+v", cmds)
	}
	var data runnerproto.TurnStartData
	_ = json.Unmarshal(cmds[1].Data, &data)
	if len(data.Input) != 1 || data.Input[0].Content != "again" || data.Input[0].Sender.ID != "u1" || data.TurnID == turn {
		t.Errorf("data = %+v", data)
	}
}

func TestMidTurn(t *testing.T) {
	e := newEnv(t)
	run, turn := liveRun(t, e)
	e.updateSession(run.ID, 1, func(s *core.RunSession) { s.Capabilities = []string{string(runnerproto.CapMidTurnInput)} })
	out, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "user", Content: "extra"})
	if err != nil || out != OutcomeLiveSession {
		t.Fatalf("send = %q, %v", out, err)
	}
	cmds := e.commands(run.ID, 1)
	if len(cmds) != 2 || cmds[1].Type != string(runnerproto.CommandMessageDeliver) {
		t.Fatalf("commands = %+v", cmds)
	}
	in := e.inputs(run.ID, core.RunInputDelivered)
	if len(in) != 2 || in[1].TurnID != turn {
		t.Errorf("delivered = %+v", in)
	}
}

func TestMidTurnWithoutCapability(t *testing.T) {
	e := newEnv(t)
	run, _ := liveRun(t, e)
	out, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "user", Content: "extra"})
	if err != nil || out != OutcomeLiveSession {
		t.Fatalf("send = %q, %v", out, err)
	}
	if n := len(e.commands(run.ID, 1)); n != 1 {
		t.Errorf("commands = %d", n)
	}
	if in := e.inputs(run.ID, core.RunInputPending); len(in) != 1 || in[0].Content != "extra" {
		t.Errorf("pending = %+v", in)
	}
}

func TestSendToFinishedRunStartsNewSession(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunSucceeded, "p")
	if err := e.mem.CreateRunSession(e.ctx, &core.RunSession{RunID: run.ID, Session: 1, TaskID: core.TaskID(run.ID, 1)}); err != nil {
		t.Fatal(err)
	}
	e.endSessionRaw(run.ID, 1)
	out, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "user", Content: "more"})
	if err != nil || out != OutcomeNewSession {
		t.Fatalf("send = %q, %v", out, err)
	}
	if got := e.run(run.ID).State; got != core.RunQueued {
		t.Fatalf("state = %s", got)
	}
	e.tick()
	if _, err := e.mem.GetRunSession(e.ctx, run.ID, 2); err != nil {
		t.Fatalf("session 2: %v", err)
	}
}

func TestSendQueuedWhileSetup(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick() // pending
	for _, want := range []core.RunState{core.RunPending, core.RunClaimed} {
		if got := e.run(run.ID).State; got != want {
			t.Fatalf("state = %s, want %s", got, want)
		}
		out, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "user", Content: "early"})
		if err != nil || out != OutcomeQueued {
			t.Fatalf("send = %q, %v", out, err)
		}
		e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
	}
}

func TestSendDedupe(t *testing.T) {
	e := newEnv(t)
	run, _ := liveRun(t, e)
	m := func() *core.RunInput {
		return &core.RunInput{RunID: run.ID, Kind: "event", Content: "x", DedupeKey: "k1"}
	}
	out1, err := e.d.Send(e.ctx, m())
	if err != nil || out1 != OutcomeLiveSession {
		t.Fatalf("first = %q, %v", out1, err)
	}
	calls := e.cmd.calls
	out2, err := e.d.Send(e.ctx, m())
	if err != nil || out2 != OutcomeLiveSession {
		t.Fatalf("second = %q, %v", out2, err)
	}
	n := 0
	for _, in := range e.inputs(run.ID) {
		if in.DedupeKey == "k1" {
			n++
		}
	}
	if n != 1 || e.cmd.calls != calls {
		t.Errorf("messages = %d, commands %d -> %d", n, calls, e.cmd.calls)
	}
}

func TestLostSession(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	for i := 1; i <= core.MaxRetrySessions; i++ {
		e.tick()
		e.makeActive(run.ID, i)
		e.tick() // delivers the prompt
		if len(e.inputs(run.ID, core.RunInputDelivered)) != 1 {
			t.Fatalf("session %d: prompt not delivered", i)
		}
		e.advance(2 * time.Minute)
		e.backend.setStatus(core.TaskStatus{TaskID: core.TaskID(run.ID, i), State: core.TaskExited, Reason: core.SandboxEvicted})
		e.tick()

		s := e.session(run.ID, i)
		if s.State != core.SessionEnded || s.EndReason != "lost" || s.SandboxReason != core.SandboxEvicted || s.EndedAt == nil {
			t.Fatalf("session %d = %+v", i, s)
		}
		if len(e.inputs(run.ID, core.RunInputPending)) != 1 {
			t.Fatalf("session %d: prompt not back to pending", i)
		}
		want := core.RunQueued
		if i == core.MaxRetrySessions {
			want = core.RunError
		}
		if got := e.run(run.ID).State; got != want {
			t.Fatalf("after loss %d: state = %s, want %s", i, got, want)
		}
	}
	if r := e.run(run.ID); r.Error != string(core.FailureSandboxLost) {
		t.Errorf("error = %q", r.Error)
	}
	if d := e.backend.destroyedIDs(); len(d) != core.MaxRetrySessions || d[0] != run.ID+".1" {
		t.Errorf("destroyed = %v", d)
	}
}

func TestLostExpiredBootstrapUnknownTask(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	delete(e.backend.statuses, run.ID+".1") // the backend forgot the task
	e.advance(31 * time.Minute)
	e.tick()
	s := e.session(run.ID, 1)
	if s.EndReason != "lost" || s.SandboxReason != core.SandboxDeadlineExceeded {
		t.Fatalf("session = %+v", s)
	}
	// The start timeout gives sandbox_create_failed, a retry code.
	if !slices.Contains(e.backend.cancelled, run.ID+".1") {
		t.Errorf("cancelled = %v", e.backend.cancelled)
	}
	if got := e.run(run.ID).State; got != core.RunQueued {
		t.Errorf("state = %s", got)
	}
}

func TestSilentButRunning(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	e.makeActive(run.ID, 1)
	e.backend.setStatus(core.TaskStatus{TaskID: run.ID + ".1", State: core.TaskRunning})
	// Silent, but within the second wait: the session stays.
	e.advance(e.d.cfg.LostAfter + 30*time.Second)
	e.tick()
	if s := e.session(run.ID, 1); s.State != core.SessionActive {
		t.Errorf("session = %+v", s)
	}
	if len(e.backend.destroyedIDs()) != 0 {
		t.Error("destroyed")
	}
	// After the second wait the task is cancelled and the run gets runner_lost.
	e.advance(e.d.cfg.LostAfter)
	e.tick()
	s := e.session(run.ID, 1)
	if s.State != core.SessionEnded || s.EndReason != "lost" {
		t.Fatalf("session = %s %s", s.State, s.EndReason)
	}
	if !slices.Contains(e.backend.cancelled, run.ID+".1") {
		t.Errorf("cancelled = %v", e.backend.cancelled)
	}
	if got := e.run(run.ID).State; got != core.RunQueued {
		t.Errorf("state = %s (runner_lost retries)", got)
	}
}

func TestSessionEnded(t *testing.T) {
	t.Run("setup_failed", func(t *testing.T) {
		e := newEnv(t)
		run := e.newRun(core.RunQueued, "p")
		e.tick()
		e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
		e.d.SessionEnded(e.ctx, e.session(run.ID, 1), runnerproto.EndRequest{Reason: runnerproto.EndSetupFailed, FailureCode: "environment_setup_failed"})
		r := e.run(run.ID)
		if r.State != core.RunFailed || r.Error != "environment_setup_failed" {
			t.Errorf("run = %+v", r)
		}
		if d := e.backend.destroyedIDs(); len(d) != 1 || d[0] != run.ID+".1" {
			t.Errorf("destroyed = %v", d)
		}
	})
	t.Run("setup_failed with step output", func(t *testing.T) {
		e := newEnv(t)
		run := e.newRun(core.RunQueued, "p")
		e.tick()
		e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
		msg := "step command:1 failed: exit status 3\nnpm ERR! missing script: build"
		e.d.SessionEnded(e.ctx, e.session(run.ID, 1), runnerproto.EndRequest{Reason: runnerproto.EndSetupFailed, FailureCode: "environment_setup_failed", Message: msg})
		r := e.run(run.ID)
		if r.State != core.RunFailed || r.FailureCode != core.FailureEnvironmentSetupFailed || r.Error != "environment_setup_failed: "+msg {
			t.Errorf("run = %+v", r)
		}
	})
	t.Run("setup_failed message is bounded", func(t *testing.T) {
		e := newEnv(t)
		run := e.newRun(core.RunQueued, "p")
		e.tick()
		e.d.SessionStarted(e.ctx, e.session(run.ID, 1))
		e.d.SessionEnded(e.ctx, e.session(run.ID, 1), runnerproto.EndRequest{Reason: runnerproto.EndSetupFailed, FailureCode: "environment_setup_failed", Message: strings.Repeat("x", 100000)})
		if n := len(e.run(run.ID).Error); n > maxFailureMessage+64 {
			t.Errorf("error has %d bytes", n)
		}
	})
	t.Run("idle after success", func(t *testing.T) {
		e := newEnv(t)
		run, turn := liveRun(t, e)
		s := e.session(run.ID, 1)
		e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil),
			event(t, 2, runnerproto.EventTurnEnded, turn, runnerproto.TurnEndedData{Outcome: runnerproto.OutcomeDone}))
		e.d.SessionEnded(e.ctx, s, runnerproto.EndRequest{Reason: runnerproto.EndIdle})
		if got := e.run(run.ID).State; got != core.RunSucceeded {
			t.Errorf("state = %s", got)
		}
	})
	t.Run("stopped while running", func(t *testing.T) {
		e := newEnv(t)
		run, turn := liveRun(t, e)
		s := e.session(run.ID, 1)
		e.events(s, event(t, 1, runnerproto.EventTurnStarted, turn, nil))
		e.endSessionRaw(run.ID, 1)
		e.d.SessionEnded(e.ctx, e.session(run.ID, 1), runnerproto.EndRequest{Reason: runnerproto.EndStopped})
		// runner_lost is a retry code and this is the first loss: queued.
		if got := e.run(run.ID).State; got != core.RunQueued {
			t.Errorf("state = %s", got)
		}
	})
	t.Run("fatal", func(t *testing.T) {
		e := newEnv(t)
		run, turn := liveRun(t, e)
		e.events(e.session(run.ID, 1), event(t, 1, runnerproto.EventTurnStarted, turn, nil))
		e.d.SessionEnded(e.ctx, e.session(run.ID, 1), runnerproto.EndRequest{Reason: runnerproto.EndFatal, FailureCode: "agent_failed"})
		r := e.run(run.ID)
		if r.State != core.RunError || r.Error != string(core.FailureAgentProcessFailed) {
			t.Errorf("run = %+v", r)
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t)
		run, _ := liveRun(t, e)
		before := e.run(run.ID).State
		e.d.SessionEnded(e.ctx, e.session(run.ID, 1), runnerproto.EndRequest{Reason: runnerproto.EndCancelled})
		if got := e.run(run.ID).State; got != before {
			t.Errorf("state = %s", got)
		}
	})
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	run, _ := liveRun(t, e)
	if err := e.d.Cancel(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	cmds := e.commands(run.ID, 1)
	if last := cmds[len(cmds)-1]; last.Type != string(runnerproto.CommandSessionCancel) {
		t.Fatalf("commands = %+v", cmds)
	}
	if got := e.run(run.ID).State; got != core.RunCancelled {
		t.Errorf("state = %s", got)
	}
	if len(e.backend.cancelled) != 0 {
		t.Error("backend cancelled an active session")
	}
}

func TestStopSession(t *testing.T) {
	e := newEnv(t)
	run, _ := liveRun(t, e)
	before := e.run(run.ID).State
	if err := e.d.StopSession(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	cmds := e.commands(run.ID, 1)
	if last := cmds[len(cmds)-1]; last.Type != string(runnerproto.CommandSessionCancel) {
		t.Fatalf("commands = %+v", cmds)
	}
	if got := e.run(run.ID).State; got != before {
		t.Errorf("state changed from %s to %s", before, got)
	}
	// A run without a session is not an error.
	q := e.newRun(core.RunQueued, "p")
	if err := e.d.StopSession(e.ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.run(q.ID).State; got != core.RunQueued {
		t.Errorf("queued run state = %s", got)
	}
}

func TestStopSessionDuringSetup(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	// The session is active but not ready: the runner is in its setup.
	e.updateSession(run.ID, 1, func(s *core.RunSession) {
		now := *e.clock
		s.State = core.SessionActive
		s.StartedAt = &now
	})
	if err := e.d.StopSession(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if len(e.backend.cancelled) != 1 || e.backend.cancelled[0] != run.ID+".1" {
		t.Errorf("cancelled = %v", e.backend.cancelled)
	}
	// A runner stopped during its session start cannot send its end, so the
	// dispatcher ends the session.
	if s := e.session(run.ID, 1); s.State != core.SessionEnded || s.EndReason != string(runnerproto.EndCancelled) {
		t.Errorf("session = %s (%s), want ended (cancelled)", s.State, s.EndReason)
	}
}

func TestCancelAssignedAndQueued(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	if err := e.d.Cancel(e.ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.run(run.ID).State; got != core.RunCancelled {
		t.Errorf("state = %s", got)
	}
	if len(e.backend.cancelled) != 1 || e.backend.cancelled[0] != run.ID+".1" {
		t.Errorf("cancelled = %v", e.backend.cancelled)
	}
	q := e.newRun(core.RunQueued, "p")
	if err := e.d.Cancel(e.ctx, q.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.run(q.ID).State; got != core.RunCancelled {
		t.Errorf("queued run state = %s", got)
	}
}

func TestCommanderFailures(t *testing.T) {
	t.Run("failing", func(t *testing.T) {
		e := newEnv(t)
		e.cmd.err = errors.New("down")
		run := e.newRun(core.RunQueued, "p")
		e.tick()
		e.makeActive(run.ID, 1)
		e.tick()
		if in := e.inputs(run.ID, core.RunInputPending); len(in) != 1 {
			t.Errorf("pending = %d", len(in))
		}
		if got := e.session(run.ID, 1).ActiveTurnID; got != "" {
			t.Errorf("turn not released: %q", got)
		}
		if err := e.d.Cancel(e.ctx, run.ID); err != nil {
			t.Error(err)
		}
	})
	t.Run("nil", func(t *testing.T) {
		e := newEnv(t)
		e.d.SetCommander(nil)
		run := e.newRun(core.RunQueued, "p")
		e.tick()
		e.makeActive(run.ID, 1)
		e.tick()
		if in := e.inputs(run.ID, core.RunInputPending); len(in) != 1 {
			t.Errorf("pending = %d", len(in))
		}
		if got := e.session(run.ID, 1).ActiveTurnID; got != "" {
			t.Errorf("turn claimed without commander: %q", got)
		}
		if _, err := e.d.Send(e.ctx, &core.RunInput{RunID: run.ID, Kind: "user", Content: "x"}); err != nil {
			t.Error(err)
		}
		if err := e.d.Cancel(e.ctx, run.ID); err != nil {
			t.Error(err)
		}
	})
}

func TestCallbacks(t *testing.T) {
	e := newEnv(t)
	var runs, msgs int
	d, err := New(Config{Store: e.mem, Runner: e.mem, Backend: e.backend, ControlURL: "http://x", Now: func() time.Time { return *e.clock },
		OnRunChanged: func(context.Context, *core.Run) { runs++ },
		OnRunMessage: func(context.Context, *core.RunMessage) { msgs++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	e.d = d
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	e.events(e.session(run.ID, 1), event(t, 1, runnerproto.EventMessage, "", runnerproto.MessageData{Role: "assistant", Content: "hi"}))
	if runs != 1 || msgs != 1 {
		t.Errorf("runs = %d, msgs = %d", runs, msgs)
	}
}

func TestInterrupt(t *testing.T) {
	reason := func(err error) string {
		var ie *core.InterruptError
		if errors.As(err, &ie) {
			return ie.Reason
		}
		return ""
	}
	t.Run("no session", func(t *testing.T) {
		e := newEnv(t)
		run := e.newRun(core.RunQueued, "start")
		err := e.d.Interrupt(e.ctx, run.ID)
		if reason(err) != core.InterruptNoActiveTurn || !errors.Is(err, core.ErrConflict) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no active turn", func(t *testing.T) {
		e := newEnv(t)
		run, _ := liveRun(t, e)
		e.updateSession(run.ID, 1, func(s *core.RunSession) { s.ActiveTurnID = "" })
		if err := e.d.Interrupt(e.ctx, run.ID); reason(err) != core.InterruptNoActiveTurn {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("without capability", func(t *testing.T) {
		e := newEnv(t)
		run, _ := liveRun(t, e)
		if err := e.d.Interrupt(e.ctx, run.ID); reason(err) != core.InterruptUnsupportedHarness {
			t.Fatalf("err = %v", err)
		}
		if n := len(e.commands(run.ID, 1)); n != 1 {
			t.Errorf("commands = %d", n)
		}
	})
	t.Run("with capability", func(t *testing.T) {
		e := newEnv(t)
		run, turn := liveRun(t, e)
		e.updateSession(run.ID, 1, func(s *core.RunSession) { s.Capabilities = []string{string(runnerproto.CapInterrupt)} })
		if err := e.d.Interrupt(e.ctx, run.ID); err != nil {
			t.Fatal(err)
		}
		cmds := e.commands(run.ID, 1)
		if len(cmds) != 2 || cmds[1].Type != string(runnerproto.CommandTurnInterrupt) {
			t.Fatalf("commands = %+v", cmds)
		}
		var data runnerproto.TurnInterruptData
		if err := json.Unmarshal(cmds[1].Data, &data); err != nil || data.TurnID != turn {
			t.Errorf("data = %+v, %v", data, err)
		}
	})
	t.Run("no commander", func(t *testing.T) {
		e := newEnv(t)
		run, _ := liveRun(t, e)
		e.updateSession(run.ID, 1, func(s *core.RunSession) { s.Capabilities = []string{string(runnerproto.CapInterrupt)} })
		e.d.SetCommander(nil)
		if err := e.d.Interrupt(e.ctx, run.ID); err == nil || reason(err) != "" {
			t.Fatalf("err = %v", err)
		}
	})
}

type uploadCall struct {
	run, kind, name, id string
	session             int
}

func TestUploadEvents(t *testing.T) {
	e := newEnv(t)
	var calls []uploadCall
	d, err := New(Config{Store: e.mem, Runner: e.mem, Backend: e.backend, ControlURL: "http://x", Now: func() time.Time { return *e.clock },
		OnUpload: func(_ context.Context, runID string, session int, kind, name, id string) error {
			calls = append(calls, uploadCall{runID, kind, name, id, session})
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.d = d
	run := e.newRun(core.RunQueued, "p")
	e.tick()
	s := e.session(run.ID, 1)
	e.events(s,
		event(t, 1, runnerproto.EventArtifact, "", runnerproto.ArtifactData{Name: "out/report.html", Type: "FILE", UploadID: "up_a"}),
		event(t, 2, runnerproto.EventTranscriptSaved, "", runnerproto.TranscriptSavedData{UploadID: "up_t"}),
	)
	want := []uploadCall{
		{run.ID, "artifact", "out/report.html", "up_a", 1},
		{run.ID, "transcript", "", "up_t", 1},
	}
	if len(calls) != 2 || calls[0] != want[0] || calls[1] != want[1] {
		t.Fatalf("calls = %+v, want %+v", calls, want)
	}
	msgs, err := e.mem.ListRunMessages(e.ctx, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Role != "artifact" || msgs[0].Content != "out/report.html" {
		t.Errorf("messages = %+v", msgs)
	}
}

// The engine puts the first prompt in the queue when it creates the run. A
// message that arrives before the first claim then comes after the prompt, and
// the claim does not queue the prompt a second time.
func TestFirstPromptKeepsOrderBeforeClaim(t *testing.T) {
	e := newEnv(t)
	run := e.newRun(core.RunQueued, "first")
	if _, err := e.d.Send(e.ctx, &core.RunInput{ID: "msg_first", RunID: run.ID, Kind: "user", Content: "first",
		Sender: core.RunInputSender{Type: "user", ID: "u1"}, DedupeKey: "prompt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.d.Send(e.ctx, &core.RunInput{ID: "msg_second", RunID: run.ID, Kind: "user", Content: "second",
		Sender: core.RunInputSender{Type: "user", ID: "u1"}, DedupeKey: "msg:msg_second"}); err != nil {
		t.Fatal(err)
	}
	e.tick()
	in := e.inputs(run.ID)
	if len(in) != 2 || in[0].ID != "msg_first" || in[0].Content != "first" || in[1].ID != "msg_second" || in[0].Seq >= in[1].Seq {
		t.Fatalf("inputs = %+v", in)
	}
}
