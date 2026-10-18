package wire

import (
	"errors"
	"math"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/openfactory-org/openfactory/internal/core"
	pbv2 "github.com/openfactory-org/openfactory/pkg/plugin/pb/v2"
)

// CapSandboxBackend is the capability name of a plugin that serves the version 2
// SandboxBackend service (openfactory.plugin.v2.SandboxBackend).
const CapSandboxBackend = "sandbox_backend"

// ClampInt32 converts v to int32, clamped to the int32 range.
func ClampInt32(v int) int32 {
	if v > math.MaxInt32 {
		return math.MaxInt32
	}
	if v < math.MinInt32 {
		return math.MinInt32
	}
	return int32(v)
}

// SandboxSpecToPB converts a core.Task.
func SandboxSpecToPB(t core.Task) *pbv2.SandboxSpec {
	out := &pbv2.SandboxSpec{
		Id: t.TaskID, RunId: t.RunID, Session: ClampInt32(t.Session), TeamId: t.TeamID, FactoryId: t.FactoryID,
		Image: t.Image, RunnerImage: t.RunnerImage,
		Platform:  &pbv2.SandboxPlatform{Os: t.Platform.OS, Arch: t.Platform.Arch},
		Resources: &pbv2.SandboxResources{Cpu: t.Resources.CPU, Memory: t.Resources.Memory},
		Env:       t.Env, MaxSessionSeconds: ClampInt32(t.MaxSessionSeconds), FailureRetentionSeconds: ClampInt32(t.FailureRetentionSeconds),
		WorkspaceSizeLimit: t.WorkspaceSizeLimit, Labels: t.Labels,
	}
	for _, im := range t.ToolImages {
		out.ToolImages = append(out.ToolImages, &pbv2.ToolImage{Name: im.Name, Image: im.Image})
	}
	if np := t.NetworkPolicy; np != nil {
		out.NetworkPolicy = &pbv2.SandboxNetworkPolicy{Mode: np.Mode, AllowedHosts: np.AllowedHosts, DeniedHosts: np.DeniedHosts}
	}
	return out
}

// TaskFromPB converts a pbv2.SandboxSpec. A nil message gives the zero task.
func SandboxSpecFromPB(t *pbv2.SandboxSpec) core.Task {
	if t == nil {
		return core.Task{}
	}
	out := core.Task{
		TaskID: t.Id, RunID: t.RunId, Session: int(t.Session), TeamID: t.TeamId, FactoryID: t.FactoryId,
		Image: t.Image, RunnerImage: t.RunnerImage,
		Platform:  core.TaskPlatform{OS: t.GetPlatform().GetOs(), Arch: t.GetPlatform().GetArch()},
		Resources: core.TaskResources{CPU: t.GetResources().GetCpu(), Memory: t.GetResources().GetMemory()},
		Env:       t.Env, MaxSessionSeconds: int(t.MaxSessionSeconds), FailureRetentionSeconds: int(t.FailureRetentionSeconds),
		WorkspaceSizeLimit: t.WorkspaceSizeLimit, Labels: t.Labels,
	}
	for _, im := range t.ToolImages {
		out.ToolImages = append(out.ToolImages, core.TaskImage{Name: im.Name, Image: im.Image})
	}
	if np := t.NetworkPolicy; np != nil {
		out.NetworkPolicy = &core.TaskNetworkPolicy{Mode: np.Mode, AllowedHosts: np.AllowedHosts, DeniedHosts: np.DeniedHosts}
	}
	return out
}

func tsToPB(t *time.Time) *timestamppb.Timestamp {
	if t == nil {
		return nil
	}
	return timestamppb.New(*t)
}

func tsFromPB(t *timestamppb.Timestamp) *time.Time {
	if t == nil {
		return nil
	}
	v := t.AsTime()
	return &v
}

// SandboxStatusToPB converts a core.TaskStatus.
func SandboxStatusToPB(s core.TaskStatus) *pbv2.SandboxStatus {
	out := &pbv2.SandboxStatus{
		Id: s.TaskID, State: string(s.State), Reason: string(s.Reason), Message: s.Message, BackendName: s.SandboxID,
		StartedAt: tsToPB(s.StartedAt), EndedAt: tsToPB(s.EndedAt),
	}
	if s.ExitCode != nil {
		c := ClampInt32(*s.ExitCode)
		out.ExitCode = &c
	}
	return out
}

// TaskStatusFromPB converts a pbv2.SandboxStatus. A nil message gives the zero status.
func SandboxStatusFromPB(s *pbv2.SandboxStatus) core.TaskStatus {
	if s == nil {
		return core.TaskStatus{}
	}
	out := core.TaskStatus{
		TaskID: s.Id, State: core.TaskState(s.State), Reason: core.SandboxReason(s.Reason), Message: s.Message,
		SandboxID: s.BackendName, StartedAt: tsFromPB(s.StartedAt), EndedAt: tsFromPB(s.EndedAt),
	}
	if s.ExitCode != nil {
		c := int(*s.ExitCode)
		out.ExitCode = &c
	}
	return out
}

// SandboxErrToStatus maps an error of a sandbox backend to a gRPC status error:
// core.ErrAtCapacity gives RESOURCE_EXHAUSTED, the other sentinels give the
// codes of ToStatus (core.ErrNotFound gives NOT_FOUND, core.ErrInvalid gives
// INVALID_ARGUMENT).
func SandboxErrToStatus(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	if errors.Is(err, core.ErrAtCapacity) {
		return status.Error(codes.ResourceExhausted, err.Error())
	}
	return ToStatus(err)
}

// SandboxErrFromStatus maps a gRPC error of a sandbox backend plugin to a Go error.
// NOT_FOUND matches core.ErrNotFound, INVALID_ARGUMENT matches core.ErrInvalid
// and RESOURCE_EXHAUSTED matches core.ErrAtCapacity. UNAVAILABLE stays a
// *RemoteError whose Temporary method returns true.
func SandboxErrFromStatus(plugin string, err error) error {
	err = FromStatus(plugin, err)
	var re *RemoteError
	if errors.As(err, &re) && re.Code == codes.ResourceExhausted {
		return &capacityError{RemoteError: re}
	}
	return err
}

// capacityError is a RemoteError that also matches core.ErrAtCapacity.
type capacityError struct{ *RemoteError }

func (e *capacityError) Unwrap() []error { return []error{e.RemoteError, core.ErrAtCapacity} }
