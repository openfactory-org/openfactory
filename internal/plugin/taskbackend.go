package plugin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	pbv2 "github.com/openfactory-org/openfactory/pkg/plugin/pb/v2"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// watchBuffer is the size of the channel that Watch returns.
const watchBuffer = 16

// TaskBackend returns the core.TaskBackend of the named plugin. The plugin
// must serve the version 2 TaskBackend service (openfactory.plugin.v2). A
// plugin that serves only the version 1 SandboxBackend service gives an error,
// because the runner execution path needs protocol version 2.
func (h *Host) TaskBackend(name string) (core.TaskBackend, error) {
	in, ok := h.plugin(name)
	if !ok {
		return nil, fmt.Errorf("plugin %q is not loaded: check OF_PLUGIN_DIR, OF_PLUGINS and the plugin log", name)
	}
	info := in.pluginInfo()
	if !hasCap(info, wire.CapSandboxBackend) {
		return nil, fmt.Errorf("plugin %s has no SandboxBackend service; the server needs plugin protocol v2", name)
	}
	return &pluginTaskBackend{h: h, in: in, name: "plugin:" + info.Name}, nil
}

// pluginTaskBackend implements core.TaskBackend by calling a plugin.
type pluginTaskBackend struct {
	h    *Host
	in   *instance
	name string
}

var _ core.TaskBackend = (*pluginTaskBackend)(nil)

func (b *pluginTaskBackend) Name() string { return b.name }

// client returns a client and a context with the call timeout.
func (b *pluginTaskBackend) client(ctx context.Context) (pbv2.SandboxBackendClient, context.Context, context.CancelFunc, error) {
	conn, err := b.in.conn()
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := b.h.callCtx(ctx)
	return pbv2.NewSandboxBackendClient(conn), ctx, cancel, nil
}

func (b *pluginTaskBackend) err(err error) error {
	return wire.SandboxErrFromStatus(b.in.name(), err)
}

func (b *pluginTaskBackend) Start(ctx context.Context, task core.Task) (core.TaskStatus, error) {
	c, ctx, cancel, err := b.client(ctx)
	if err != nil {
		return core.TaskStatus{}, err
	}
	defer cancel()
	st, err := c.StartSandbox(ctx, wire.SandboxSpecToPB(task))
	if err != nil {
		return core.TaskStatus{}, b.err(err)
	}
	return wire.SandboxStatusFromPB(st), nil
}

func (b *pluginTaskBackend) Status(ctx context.Context, taskID string) (core.TaskStatus, error) {
	c, ctx, cancel, err := b.client(ctx)
	if err != nil {
		return core.TaskStatus{}, err
	}
	defer cancel()
	st, err := c.GetSandbox(ctx, &pbv2.SandboxRef{Id: taskID})
	if err != nil {
		return core.TaskStatus{}, b.err(err)
	}
	return wire.SandboxStatusFromPB(st), nil
}

// Watch opens the stream and waits for its first status, so that an error of
// the plugin, such as NOT_FOUND, comes back as the error of the call. The
// plugin must send the current status first. The channel closes when the
// plugin ends the stream, when the stream fails or when ctx ends. A caller
// that needs to know why the channel closed can call Status.
func (b *pluginTaskBackend) Watch(ctx context.Context, taskID string) (<-chan core.TaskStatus, error) {
	conn, err := b.in.conn()
	if err != nil {
		return nil, err
	}
	sctx, cancel := context.WithCancel(ctx)
	stream, err := pbv2.NewSandboxBackendClient(conn).WatchSandbox(sctx, &pbv2.SandboxRef{Id: taskID})
	if err != nil {
		cancel()
		return nil, b.err(err)
	}
	// Bound the wait for the first status with the call timeout.
	timer := time.AfterFunc(b.h.opts.CallTimeout, cancel)
	first, err := stream.Recv()
	timer.Stop()
	if err != nil {
		cancel()
		if errors.Is(err, io.EOF) {
			ch := make(chan core.TaskStatus)
			close(ch)
			return ch, nil
		}
		return nil, b.err(err)
	}
	ch := make(chan core.TaskStatus, watchBuffer)
	ch <- wire.SandboxStatusFromPB(first)
	go func() {
		defer close(ch)
		defer cancel()
		for {
			m, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case ch <- wire.SandboxStatusFromPB(m):
			case <-sctx.Done():
				return
			}
		}
	}()
	return ch, nil
}

func (b *pluginTaskBackend) Cancel(ctx context.Context, taskID string, graceSeconds int) error {
	c, ctx, cancel, err := b.client(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.CancelSandbox(ctx, &pbv2.CancelRequest{Id: taskID, GraceSeconds: wire.ClampInt32(graceSeconds)})
	return b.err(err)
}

func (b *pluginTaskBackend) Destroy(ctx context.Context, taskID string) error {
	c, ctx, cancel, err := b.client(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.DestroySandbox(ctx, &pbv2.SandboxRef{Id: taskID})
	return b.err(err)
}

func (b *pluginTaskBackend) List(ctx context.Context) ([]core.TaskStatus, error) {
	c, ctx, cancel, err := b.client(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	l, err := c.ListSandboxes(ctx, &pbv2.Empty{})
	if err != nil {
		return nil, b.err(err)
	}
	out := make([]core.TaskStatus, 0, len(l.Sandboxes))
	for _, t := range l.Sandboxes {
		out = append(out, wire.SandboxStatusFromPB(t))
	}
	return out, nil
}

func (b *pluginTaskBackend) Preflight(ctx context.Context) error {
	c, ctx, cancel, err := b.client(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	_, err = c.Preflight(ctx, &pbv2.Empty{})
	return b.err(err)
}
