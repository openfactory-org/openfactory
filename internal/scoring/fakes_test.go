package scoring

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// fakeStore is an in-memory core.Store with just the methods scoring uses.
// Unimplemented methods panic through the nil embedded interface.
type fakeStore struct {
	core.Store
	mu         sync.Mutex
	teams      []core.Team
	factories  map[string]*core.Factory
	versions   map[string]*core.DefinitionVersion
	scorers    map[string][]core.Scorer
	scores     []core.Score
	runs       map[string]*core.Run
	messages   map[string][]core.RunMessage
	users      []core.User
	inbox      []core.InboxItem
	benchmarks map[string]*core.BenchmarkRun
	workItems  map[string]*core.WorkItem
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		factories:  map[string]*core.Factory{},
		versions:   map[string]*core.DefinitionVersion{},
		scorers:    map[string][]core.Scorer{},
		runs:       map[string]*core.Run{},
		messages:   map[string][]core.RunMessage{},
		benchmarks: map[string]*core.BenchmarkRun{},
		workItems:  map[string]*core.WorkItem{},
	}
}

func (f *fakeStore) putWorkItem(w *core.WorkItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *w
	f.workItems[w.ID] = &cp
}

func (f *fakeStore) GetWorkItem(ctx context.Context, id string) (*core.WorkItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.workItems[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	cp := *w
	return &cp, nil
}

func (f *fakeStore) ListTeams(ctx context.Context) ([]core.Team, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Team(nil), f.teams...), nil
}

func (f *fakeStore) ListFactories(ctx context.Context, teamID, search string) ([]core.Factory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Factory
	for _, x := range f.factories {
		if x.TeamID == teamID {
			out = append(out, *x)
		}
	}
	return out, nil
}

func (f *fakeStore) GetFactory(ctx context.Context, id string) (*core.Factory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	x, ok := f.factories[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	cp := *x
	return &cp, nil
}

func (f *fakeStore) GetDefinitionVersion(ctx context.Context, id string) (*core.DefinitionVersion, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.versions[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	cp := *v
	return &cp, nil
}

func (f *fakeStore) ListScorers(ctx context.Context, factoryID string) ([]core.Scorer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Scorer(nil), f.scorers[factoryID]...), nil
}

func (f *fakeStore) CreateScore(ctx context.Context, s *core.Score) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scores = append(f.scores, *s)
	return nil
}

func (f *fakeStore) UpdateScore(ctx context.Context, s *core.Score) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.scores {
		if f.scores[i].ID == s.ID {
			f.scores[i] = *s
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fakeStore) ListScores(ctx context.Context, factoryID, scorerID string, since *time.Time) ([]core.Score, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := map[string]bool{}
	for _, sc := range f.scorers[factoryID] {
		ids[sc.ID] = true
	}
	var out []core.Score
	for _, s := range f.scores {
		if !ids[s.ScorerID] || (scorerID != "" && s.ScorerID != scorerID) {
			continue
		}
		if since != nil && s.CreatedAt.Before(*since) {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (f *fakeStore) GetScore(ctx context.Context, scorerID, runID string) (*core.Score, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.scores) - 1; i >= 0; i-- {
		if f.scores[i].ScorerID == scorerID && f.scores[i].RunID == runID {
			s := f.scores[i]
			return &s, nil
		}
	}
	return nil, core.ErrNotFound
}

func (f *fakeStore) putRun(r *core.Run) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *r
	f.runs[r.ID] = &cp
}

func (f *fakeStore) GetRun(ctx context.Context, id string) (*core.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeStore) ListRuns(ctx context.Context, flt core.RunFilter) ([]core.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Run
	for _, r := range f.runs {
		if flt.FactoryID != "" && r.FactoryID != flt.FactoryID {
			continue
		}
		if flt.ParentRunID != "" && r.ParentRunID != flt.ParentRunID {
			continue
		}
		if flt.WorkItemID != "" && r.WorkItemID != flt.WorkItemID {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeStore) addMessage(runID, role, content string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := int64(len(f.messages[runID]) + 1)
	f.messages[runID] = append(f.messages[runID], core.RunMessage{ID: core.NewID("msg"), RunID: runID, Seq: seq, Role: role, Content: content})
}

func (f *fakeStore) ListRunMessages(ctx context.Context, runID string, afterSeq int64) ([]core.RunMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.RunMessage
	for _, m := range f.messages[runID] {
		if m.Seq > afterSeq {
			out = append(out, m)
		}
	}
	// Page in chunks of two to exercise paging.
	if len(out) > 2 {
		out = out[:2]
	}
	return out, nil
}

func (f *fakeStore) ListUsers(ctx context.Context, teamID string) ([]core.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.User
	for _, u := range f.users {
		if u.TeamID == teamID {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateInboxItem(ctx context.Context, i *core.InboxItem) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inbox = append(f.inbox, *i)
	return nil
}

func (f *fakeStore) ListInboxItems(ctx context.Context, factoryID string, includeResolved bool) ([]core.InboxItem, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.InboxItem
	for _, i := range f.inbox {
		if i.FactoryID == factoryID && (includeResolved || !i.Resolved) {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *fakeStore) CreateBenchmarkRun(ctx context.Context, b *core.BenchmarkRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *b
	f.benchmarks[b.ID] = &cp
	return nil
}

func (f *fakeStore) UpdateBenchmarkRun(ctx context.Context, b *core.BenchmarkRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.benchmarks[b.ID]; !ok {
		return core.ErrNotFound
	}
	cp := *b
	f.benchmarks[b.ID] = &cp
	return nil
}

func (f *fakeStore) GetBenchmarkRun(ctx context.Context, id string) (*core.BenchmarkRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.benchmarks[id]
	if !ok {
		return nil, core.ErrNotFound
	}
	cp := *b
	return &cp, nil
}

func (f *fakeStore) ListBenchmarkRuns(ctx context.Context, factoryID string) ([]core.BenchmarkRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.BenchmarkRun
	for _, b := range f.benchmarks {
		if b.FactoryID == factoryID {
			out = append(out, *b)
		}
	}
	return out, nil
}

func (f *fakeStore) scoresFor(runID string) []core.Score {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []core.Score
	for _, s := range f.scores {
		if s.RunID == runID {
			out = append(out, s)
		}
	}
	return out
}

// fakeEngine records dispatches/cancels and materializes runs in the store.
type fakeEngine struct {
	core.Engine
	st         *fakeStore
	clock      func() time.Time
	mu         sync.Mutex
	dispatches []core.DispatchRequest
	cancels    []string
	failAgent  string
}

func (e *fakeEngine) Dispatch(ctx context.Context, req core.DispatchRequest) (*core.WorkItem, *core.Run, error) {
	e.mu.Lock()
	e.dispatches = append(e.dispatches, req)
	e.mu.Unlock()
	if e.failAgent != "" && req.AgentName == e.failAgent && strings.Contains(req.Prompt, "FAIL") {
		return nil, nil, core.ErrInvalid
	}
	wi := &core.WorkItem{ID: core.NewID("wi"), FactoryID: req.FactoryID, Title: req.Title}
	e.st.putWorkItem(wi)
	run := &core.Run{
		ID: core.NewID("run"), FactoryID: req.FactoryID, WorkItemID: wi.ID, AgentName: req.AgentName,
		State: core.RunQueued, Prompt: req.Prompt, Metadata: req.Metadata, TriggeredBy: req.TriggeredBy,
		CreatedAt: e.clock(),
	}
	if run.AgentName == "" {
		run.AgentName = "foreman"
	}
	e.st.putRun(run)
	e.st.addMessage(run.ID, "user", req.Prompt)
	return wi, run, nil
}

func (e *fakeEngine) CancelOne(ctx context.Context, runID, actor string) error {
	return e.Cancel(ctx, runID, actor)
}

func (e *fakeEngine) Cancel(ctx context.Context, runID, actor string) error {
	e.mu.Lock()
	e.cancels = append(e.cancels, runID)
	e.mu.Unlock()
	return nil
}

func (e *fakeEngine) dispatchCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.dispatches)
}

// fakeBus is a minimal synchronous fan-out bus.
type fakeBus struct {
	mu   sync.Mutex
	subs map[string][]chan any
	pubs []string
}

func newFakeBus() *fakeBus { return &fakeBus{subs: map[string][]chan any{}} }

func (b *fakeBus) Publish(ctx context.Context, topic string, payload any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pubs = append(b.pubs, topic)
	for _, ch := range b.subs[topic] {
		select {
		case ch <- payload:
		default:
		}
	}
}

func (b *fakeBus) Subscribe(topic string) (<-chan any, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan any, 64)
	b.subs[topic] = append(b.subs[topic], ch)
	return ch, func() {}
}

// fakeJudge is a scripted ClassifyFunc.
type fakeJudge struct {
	mu    sync.Mutex
	calls []judgeCall
	fn    func(model, rubric, transcript string) (string, string, error)
}

type judgeCall struct{ Model, Rubric, Transcript string }

func (j *fakeJudge) classify(ctx context.Context, model, rubric, transcript string, labels []core.ScorerLabel) (string, string, error) {
	j.mu.Lock()
	j.calls = append(j.calls, judgeCall{model, rubric, transcript})
	fn := j.fn
	j.mu.Unlock()
	return fn(model, rubric, transcript)
}

func (j *fakeJudge) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.calls)
}

type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}
