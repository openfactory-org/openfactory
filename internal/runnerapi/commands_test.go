package runnerapi

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

func TestCommandsLongPollWake(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	type result struct {
		cmds []runnerproto.Command
		err  error
		took time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		cmds, err := c.Commands(context.Background(), 0, 5)
		done <- result{cmds, err, time.Since(start)}
	}()
	time.Sleep(100 * time.Millisecond)
	data := runnerproto.TurnStartData{TurnID: "t1"}
	if _, err := e.svc.EnqueueCommand(context.Background(), e.run.ID, 1, runnerproto.CommandTurnStart, data); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.took > time.Second {
			t.Fatalf("poll took %v", r.took)
		}
		if len(r.cmds) != 1 || r.cmds[0].Seq != 1 || r.cmds[0].Type != runnerproto.CommandTurnStart {
			t.Fatalf("commands %+v", r.cmds)
		}
		var got runnerproto.TurnStartData
		if err := r.cmds[0].Decode(&got); err != nil || got.TurnID != "t1" {
			t.Fatalf("data %v %+v", err, got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("long poll did not return")
	}

	cmds, err := c.Commands(context.Background(), 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if cmds == nil || len(cmds) != 0 {
		t.Fatalf("want empty list, got %+v", cmds)
	}
	if got := e.session(1).CommandAck; got != 1 {
		t.Fatalf("CommandAck %d", got)
	}
	if n := e.svc.wake.size(); n != 0 {
		t.Fatalf("%d wake subscriptions left", n)
	}
}

func TestCommandsWaitTimeout(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	start := time.Now()
	cmds, err := c.Commands(context.Background(), 0, 1)
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if len(cmds) != 0 {
		t.Fatalf("commands %+v", cmds)
	}
	if took < 900*time.Millisecond || took > 3*time.Second {
		t.Fatalf("took %v", took)
	}
	if n := e.svc.wake.size(); n != 0 {
		t.Fatalf("%d wake subscriptions left", n)
	}
}

func TestCommandsEmptyBodyIsList(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+runnerproto.PathSessionCommands, nil)
	req.Header.Set("Authorization", "Bearer "+c.Token())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["commands"]) != "[]" {
		t.Fatalf("commands: %s", raw["commands"])
	}
}

func TestCommandsBadQuery(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	for _, q := range []string{"after=-1", "after=x", "wait=x"} {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+runnerproto.PathSessionCommands+"?"+q, nil)
		req.Header.Set("Authorization", "Bearer "+c.Token())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d", q, resp.StatusCode)
		}
	}
}

func TestCommandsPollSeesEndedSession(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	errc := make(chan error, 1)
	go func() {
		_, err := c.Commands(context.Background(), 0, 10)
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	s := e.session(1)
	s.State = core.SessionEnded
	if err := e.mem.UpdateRunSession(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("want 401")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("poll did not notice the end")
	}
}

func TestCommandsContextDoneReleasesPoll(t *testing.T) {
	e := newEnv(t, nil)
	c := e.started()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, _ = c.Commands(ctx, 0, 10)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for e.svc.wake.size() != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if n := e.svc.wake.size(); n != 0 {
		t.Fatalf("%d wake subscriptions left", n)
	}
}

func TestEnqueueCommandRejectsBadType(t *testing.T) {
	e := newEnv(t, nil)
	_, err := e.svc.EnqueueCommand(context.Background(), e.run.ID, 1, "nope", nil)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestWakeRegistry(t *testing.T) {
	w := newWakeRegistry()
	k := sessionKey{"r", 1}
	a, ua := w.subscribe(k)
	b, ub := w.subscribe(k)
	other, uo := w.subscribe(sessionKey{"r", 2})
	w.notify(k)
	for _, ch := range []<-chan struct{}{a, b} {
		select {
		case <-ch:
		default:
			t.Fatal("channel not closed")
		}
	}
	select {
	case <-other:
		t.Fatal("other session woken")
	default:
	}
	ua()
	ub()
	uo()
	if w.size() != 0 {
		t.Fatal("subscriptions left")
	}
}

// size returns the number of subscriptions.
func (w *wakeRegistry) size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, l := range w.subs {
		n += len(l)
	}
	return n
}
