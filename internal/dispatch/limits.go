package dispatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
)

// capacity counts the open sessions for one claim pass. It enforces
// Config.MaxSandboxes and Config.FactoryLimit.
//
// Known gap: the count is read from the database, but the check and the
// claim are not one atomic step. Two replicas that run a tick at the same
// time can each claim a run, so a limit can be passed by at most the number
// of replicas minus one. The limits need
// a claim query that enforces them in the database. That is not done.
type capacity struct {
	refused   bool // the count failed: claim nothing
	open      []core.RunSession
	total     int
	loaded    bool
	byFactory map[string]int
	runs      map[string]string // run ID to factory ID
	limits    map[string]int
}

func (d *Dispatcher) newCapacity(ctx context.Context) (*capacity, error) {
	open, err := d.cfg.Runner.ListOpenRunSessions(ctx, capacityBatch)
	if err != nil {
		return nil, err
	}
	// MaxSandboxes limits the tasks of the built-in backend. A session on a
	// remote worker does not count.
	local := 0
	for _, s := range open {
		if s.Backend == "" || s.Backend == d.cfg.Backend.Name() || d.cfg.Router == nil {
			local++
		}
	}
	return &capacity{
		open:      open,
		total:     local,
		byFactory: map[string]int{},
		runs:      map[string]string{},
		limits:    map[string]int{},
	}, nil
}

// allow tells whether a run can get a session now. A false result leaves the
// run queued.
func (c *capacity) allow(ctx context.Context, d *Dispatcher, run *core.Run) (bool, error) {
	if c.refused {
		return false, nil
	}
	if d.cfg.MaxSandboxes > 0 && !d.usesWorker(run) && c.total >= d.cfg.MaxSandboxes {
		return false, nil
	}
	if d.cfg.FactoryLimit == nil {
		return true, nil
	}
	limit, ok := c.limits[run.FactoryID]
	if !ok {
		var err error
		limit, err = d.cfg.FactoryLimit(ctx, run.FactoryID)
		if err != nil {
			return false, fmt.Errorf("factory limit: %w", err)
		}
		c.limits[run.FactoryID] = limit
	}
	if limit <= 0 {
		return true, nil
	}
	if err := c.load(ctx, d); err != nil {
		return false, err
	}
	return c.byFactory[run.FactoryID] < limit, nil
}

// load counts the open sessions of each factory. It reads each run once.
func (c *capacity) load(ctx context.Context, d *Dispatcher) error {
	if c.loaded {
		return nil
	}
	for _, s := range c.open {
		fid, ok := c.runs[s.RunID]
		if !ok {
			r, err := d.cfg.Store.GetRun(ctx, s.RunID)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read run %s: %w", s.RunID, err)
			}
			fid = r.FactoryID
			c.runs[s.RunID] = fid
		}
		if fid != "" {
			c.byFactory[fid]++
		}
	}
	c.loaded = true
	return nil
}

// add counts a session that this pass started.
func (c *capacity) add(d *Dispatcher, run *core.Run) {
	if !d.usesWorker(run) {
		c.total++
	}
	c.byFactory[run.FactoryID]++
}
