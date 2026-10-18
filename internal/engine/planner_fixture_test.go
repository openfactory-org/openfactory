package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// planFixture is a test environment with a Planner that uses the stores and
// the clock of the environment.
type planFixture struct {
	*TestEnv
	x     *Planner
	clock *testClock
}

func newPlanFixture(t *testing.T, modify func(*PlannerOptions), defMods ...func(*core.CompiledDefinition)) *planFixture {
	t.Helper()
	env := NewTestEnv(t, defMods...)
	clock := &testClock{t: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	env.Engine.clock = clock.Now
	opts := PlannerOptions{
		Store:      env.Store,
		Secrets:    env.Secrets,
		Forges:     FakeForges{},
		SandboxURL: "http://cp.test",
		Clock:      clock.Now,
		Logger:     DiscardLogger(),
	}
	if modify != nil {
		modify(&opts)
	}
	return &planFixture{TestEnv: env, x: NewPlanner(opts), clock: clock}
}

func (f *planFixture) run(t *testing.T, id string) *core.Run {
	t.Helper()
	r, err := f.Store.GetRun(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
