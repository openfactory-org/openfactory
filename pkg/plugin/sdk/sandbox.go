package sdk

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
	pbv2 "github.com/openfactory-org/openfactory/pkg/plugin/pb/v2"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// sandboxServer serves openfactory.plugin.v2.SandboxBackend from a core.TaskBackend.
type sandboxServer struct {
	pbv2.UnimplementedSandboxBackendServer
	b core.TaskBackend
}

func (s *sandboxServer) StartSandbox(ctx context.Context, t *pbv2.SandboxSpec) (*pbv2.SandboxStatus, error) {
	st, err := s.b.Start(ctx, wire.SandboxSpecFromPB(t))
	if err != nil {
		return nil, wire.SandboxErrToStatus(err)
	}
	return wire.SandboxStatusToPB(st), nil
}

func (s *sandboxServer) GetSandbox(ctx context.Context, r *pbv2.SandboxRef) (*pbv2.SandboxStatus, error) {
	st, err := s.b.Status(ctx, r.Id)
	if err != nil {
		return nil, wire.SandboxErrToStatus(err)
	}
	return wire.SandboxStatusToPB(st), nil
}

// WatchSandbox streams the statuses of Watch until the channel closes or the
// caller goes away.
func (s *sandboxServer) WatchSandbox(r *pbv2.SandboxRef, stream pbv2.SandboxBackend_WatchSandboxServer) error {
	ctx := stream.Context()
	ch, err := s.b.Watch(ctx, r.Id)
	if err != nil {
		return wire.SandboxErrToStatus(err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case st, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(wire.SandboxStatusToPB(st)); err != nil {
				return err
			}
		}
	}
}

func (s *sandboxServer) CancelSandbox(ctx context.Context, r *pbv2.CancelRequest) (*pbv2.Empty, error) {
	if err := s.b.Cancel(ctx, r.Id, int(r.GraceSeconds)); err != nil {
		return nil, wire.SandboxErrToStatus(err)
	}
	return &pbv2.Empty{}, nil
}

func (s *sandboxServer) DestroySandbox(ctx context.Context, r *pbv2.SandboxRef) (*pbv2.Empty, error) {
	if err := s.b.Destroy(ctx, r.Id); err != nil {
		return nil, wire.SandboxErrToStatus(err)
	}
	return &pbv2.Empty{}, nil
}

func (s *sandboxServer) ListSandboxes(ctx context.Context, _ *pbv2.Empty) (*pbv2.SandboxList, error) {
	list, err := s.b.List(ctx)
	if err != nil {
		return nil, wire.SandboxErrToStatus(err)
	}
	out := &pbv2.SandboxList{}
	for _, st := range list {
		out.Sandboxes = append(out.Sandboxes, wire.SandboxStatusToPB(st))
	}
	return out, nil
}

func (s *sandboxServer) Preflight(ctx context.Context, _ *pbv2.Empty) (*pbv2.Empty, error) {
	if err := s.b.Preflight(ctx); err != nil {
		return nil, wire.SandboxErrToStatus(err)
	}
	return &pbv2.Empty{}, nil
}
