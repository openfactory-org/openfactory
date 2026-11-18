package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runstate"
)

// recordQueue is a core.Queue that records its jobs, or fails.
type recordQueue struct {
	mu   sync.Mutex
	ids  []string
	fail error
}

func (q *recordQueue) Enqueue(_ context.Context, topic, id string, _ any) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.fail != nil {
		return q.fail
	}
	if topic == core.TopicRunTurnEnded {
		q.ids = append(q.ids, id)
	}
	return nil
}

func (q *recordQueue) Consume(string, core.JobHandler) func() { return func() {} }

func TestRunWriteEnqueuesTurnEnded(t *testing.T) {
	cases := []struct {
		name  string
		from  core.RunState
		event runstate.Event
		jobs  int
	}{
		{"cancel ends the turn", core.RunRunning, runstate.EventCancel, 1},
		{"claim does not end a turn", core.RunQueued, runstate.EventAssigned, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			q := &recordQueue{}
			e.d.cfg.Queue = q
			r := e.newRun(tc.from, "go")
			if _, _, err := e.d.transition(e.ctx, r.ID, runstate.Input{Event: tc.event}, nil); err != nil {
				t.Fatal(err)
			}
			// A write that does not change the state enqueues nothing more.
			if _, err := e.d.update(e.ctx, r.ID, func(r *core.Run) { r.Result = "x" }); err != nil {
				t.Fatal(err)
			}
			if len(q.ids) != tc.jobs {
				t.Fatalf("turn-end jobs = %v, want %d", q.ids, tc.jobs)
			}
		})
	}
}

// A failed write of the job rolls back the state change, so a turn end is
// never stored without its job.
func TestRunWriteRollsBackWithoutTurnEndedJob(t *testing.T) {
	e := newEnv(t)
	e.d.cfg.Queue = &recordQueue{fail: errors.New("outbox is full")}
	r := e.newRun(core.RunRunning, "go")
	if _, _, err := e.d.transition(e.ctx, r.ID, runstate.Input{Event: runstate.EventCancel}, nil); err == nil {
		t.Fatal("want an error when the job cannot be written")
	}
	if got := e.run(r.ID).State; got != core.RunRunning {
		t.Fatalf("state = %s, want running (rolled back)", got)
	}
}
