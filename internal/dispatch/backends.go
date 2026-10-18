package dispatch

import (
	"context"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
)

// BackendRouter gives the dispatcher the task backends of remote workers.
type BackendRouter interface {
	// BackendFor returns the backend for a run that names a worker host. A
	// false result means that no backend can take the task now. The run then
	// stays queued and gets no session.
	BackendFor(run *core.Run) (core.TaskBackend, bool)
	// Backends returns every backend that the router knows, connected or not.
	// The orphan sweep and the lookup by backend name use it.
	Backends() []core.TaskBackend
}

// builtinHost tells whether a worker host names a built-in backend and not a
// remote worker.
func builtinHost(host string) bool {
	switch host {
	case "", "server", "kubernetes", "direct", "managed":
		return true
	}
	return false
}

// usesWorker tells whether the run names a remote worker and the dispatcher
// has a router for it. A worker host that is the name of the server backend
// (for example the development backend "local") is not a remote worker.
func (d *Dispatcher) usesWorker(run *core.Run) bool {
	h := run.Config.WorkerHost
	if d.cfg.Router == nil || builtinHost(h) {
		return false
	}
	return d.cfg.Backend == nil || h != d.cfg.Backend.Name()
}

// pickBackend chooses the backend of the next task of a run. The bool is true
// when the backend comes from the router. A nil backend means that the run
// must wait in the queue.
func (d *Dispatcher) pickBackend(run *core.Run) (backend core.TaskBackend, remote bool) {
	if !d.usesWorker(run) {
		return d.cfg.Backend, false
	}
	if b, ok := d.cfg.Router.BackendFor(run); ok && b != nil {
		return b, true
	}
	return nil, true
}

// backendOf returns the backend that owns the task of a session. It uses the
// name that the session stores. A backend that the router no longer knows is
// replaced by one that answers core.ErrUnavailable.
func (d *Dispatcher) backendOf(s *core.RunSession) core.TaskBackend {
	if s.Backend == "" || d.cfg.Router == nil || s.Backend == d.cfg.Backend.Name() {
		return d.cfg.Backend
	}
	for _, b := range d.cfg.Router.Backends() {
		if b.Name() == s.Backend {
			return b
		}
	}
	return unavailableBackend{name: s.Backend}
}

// allBackends returns the built-in backend and every backend of the router.
func (d *Dispatcher) allBackends() []core.TaskBackend {
	out := []core.TaskBackend{d.cfg.Backend}
	if d.cfg.Router != nil {
		out = append(out, d.cfg.Router.Backends()...)
	}
	return out
}

// unavailableBackend stands for a backend that the dispatcher cannot reach.
type unavailableBackend struct{ name string }

func (b unavailableBackend) err() error {
	return fmt.Errorf("backend %q is not known: %w", b.name, core.ErrUnavailable)
}

func (b unavailableBackend) Name() string { return b.name }
func (b unavailableBackend) Start(context.Context, core.Task) (core.TaskStatus, error) {
	return core.TaskStatus{}, b.err()
}
func (b unavailableBackend) Status(context.Context, string) (core.TaskStatus, error) {
	return core.TaskStatus{}, b.err()
}
func (b unavailableBackend) Watch(context.Context, string) (<-chan core.TaskStatus, error) {
	return nil, b.err()
}
func (b unavailableBackend) Cancel(context.Context, string, int) error       { return b.err() }
func (b unavailableBackend) Destroy(context.Context, string) error           { return b.err() }
func (b unavailableBackend) List(context.Context) ([]core.TaskStatus, error) { return nil, b.err() }
func (b unavailableBackend) Preflight(context.Context) error                 { return b.err() }
