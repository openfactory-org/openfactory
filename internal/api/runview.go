package api

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
)

const (
	// maxRunTreeDepth is the number of levels that the API walks up or down
	// the run tree.
	maxRunTreeDepth = 10
	// maxRunTreeRuns is the number of runs that one walk down the tree reads.
	maxRunTreeRuns = 1000
	// runTreePage is the page size of the child list calls.
	runTreePage = 200
)

// runLookup reads runs and sessions for the enriched run view. It caches
// each run, session and child list for the life of one request.
type runLookup struct {
	s        *Server
	teamID   string
	runs     map[string]*core.Run // nil value: the run does not exist
	latest   map[string]*core.RunSession
	children map[string][]core.Run
}

func (s *Server) newRunLookup(teamID string) *runLookup {
	return &runLookup{s: s, teamID: teamID, runs: map[string]*core.Run{},
		latest: map[string]*core.RunSession{}, children: map[string][]core.Run{}}
}

func (l *runLookup) run(ctx context.Context, id string) *core.Run {
	if r, ok := l.runs[id]; ok {
		return r
	}
	r, err := l.s.opts.Store.GetRun(ctx, id)
	if err != nil || r.TeamID != l.teamID {
		r = nil
	}
	l.runs[id] = r
	return r
}

// rootAndDepth walks the parents of r. It stops after maxRunTreeDepth levels.
func (l *runLookup) rootAndDepth(ctx context.Context, r *core.Run) (string, int) {
	root, depth := r, 0
	for depth < maxRunTreeDepth && root.ParentRunID != "" {
		p := l.run(ctx, root.ParentRunID)
		if p == nil {
			break
		}
		root = p
		depth++
	}
	return root.ID, depth
}

// childrenOf lists the direct children of a run.
func (l *runLookup) childrenOf(ctx context.Context, id string) []core.Run {
	if c, ok := l.children[id]; ok {
		return c
	}
	var out []core.Run
	cursor := ""
	for len(out) < maxRunTreeRuns {
		page, err := l.s.opts.Store.ListRuns(ctx, core.RunFilter{TeamID: l.teamID, ParentRunID: id,
			ListOpts: core.ListOpts{Limit: runTreePage + 1, Cursor: cursor}})
		if err != nil {
			break
		}
		more := len(page) > runTreePage
		if more {
			page = page[:runTreePage]
		}
		out = append(out, page...)
		if !more {
			break
		}
		last := page[len(page)-1]
		cursor = encodeCursor(last.CreatedAt, last.ID)
	}
	l.children[id] = out
	return out
}

// descendants returns the runs under run id, breadth first. The walk is
// bounded by maxRunTreeDepth levels and maxRunTreeRuns runs.
func (l *runLookup) descendants(ctx context.Context, id string) []core.Run {
	var out []core.Run
	seen := map[string]bool{id: true}
	level := []string{id}
	for d := 0; d < maxRunTreeDepth && len(level) > 0 && len(out) < maxRunTreeRuns; d++ {
		var next []string
		for _, pid := range level {
			for _, c := range l.childrenOf(ctx, pid) {
				if seen[c.ID] || len(out) >= maxRunTreeRuns {
					continue
				}
				seen[c.ID] = true
				out = append(out, c)
				next = append(next, c.ID)
			}
		}
		level = next
	}
	return out
}

// activeChildren counts the descendants of a run that are not finished. A
// run that waits for input (idle or blocked) is not finished.
func (l *runLookup) activeChildren(ctx context.Context, id string) int {
	n := 0
	for _, c := range l.descendants(ctx, id) {
		if !c.State.Terminal() && c.State != core.RunIdle {
			n++
		}
	}
	return n
}

// latestSession returns the newest session of a run, or nil.
func (l *runLookup) latestSession(ctx context.Context, id string) *core.RunSession {
	if l.s.opts.Runner == nil {
		return nil
	}
	if ss, ok := l.latest[id]; ok {
		return ss
	}
	ss, err := l.s.opts.Runner.LatestRunSession(ctx, id)
	if err != nil {
		ss = nil
	}
	l.latest[id] = ss
	return ss
}

// runViewCtx builds the run view with the fields that need the store.
func (s *Server) runViewCtx(ctx context.Context, r *core.Run) RunView {
	return s.runViewWith(ctx, r, s.newRunLookup(r.TeamID))
}

func (s *Server) runViewWith(ctx context.Context, r *core.Run, l *runLookup) RunView {
	v := s.runView(r)
	l.runs[r.ID] = r
	v.RootRunID, v.Depth = l.rootAndDepth(ctx, r)
	v.ActiveChildRuns = l.activeChildren(ctx, r.ID)
	if ss := l.latestSession(ctx, r.ID); ss != nil {
		v.Session = ss.Session
		v.SandboxRunning = ss.State == core.SessionAssigned || ss.State == core.SessionActive
	}
	return v
}
