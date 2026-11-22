package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// Worker protocol version 2. It is
// the only version. Version 1 sent sandbox operations to the worker and was
// removed.
//
// A worker connects to ConnectPath and offers the subprotocol SubprotocolV2.
// Messages are JSON text frames with the envelope (type, id, method, params,
// result, error):
//
//	worker → hub  {"type":"hello","params":HelloV2}
//	hub → worker  {"type":"welcome","result":Welcome}
//	worker → hub  {"type":"heartbeat","params":Heartbeat}
//	hub → worker  {"type":"request","id":"…","method":"task.assign","params":{"task":Task}}
//	worker → hub  {"type":"response","id":"…","result":{"status":TaskStatus}|"error":{…}}
//	hub → worker  {"type":"request","id":"…","method":"task.cancel","params":{"task_id":"…","grace_seconds":n}}
//	hub → worker  {"type":"request","id":"…","method":"task.status"|"task.destroy","params":{"task_id":"…"}}
//	worker → hub  {"type":"event","method":"task.state","params":{"status":TaskStatus}}
//
// Extensions for enrolled workers (schemas/worker/v2/protocol.json):
//
//	hub → worker  {"type":"auth.challenge","params":AuthChallenge}   (first frame, before hello)
//	worker → hub  {"type":"auth.response","params":AuthResponse}     (Ed25519 signature)
//	hub → worker  {"type":"request","id":"…","method":"task.list","params":{}}
//	worker → hub  {"type":"response","id":"…","result":{"tasks":[HelloTask]}}
//	hub → worker  {"type":"request","id":"…","method":"drain","params":{"draining":bool}}
//	hello         facts, slots and slot size of the worker
//
// A worker in drain refuses task.assign with the error at_capacity.
const SubprotocolV2 = "openfactory.worker.v2"

// Methods and events of version 2.
const (
	methodTaskAssign  = "task.assign"
	methodTaskCancel  = "task.cancel"
	methodTaskStatus  = "task.status"
	methodTaskDestroy = "task.destroy"
	methodTaskList    = "task.list"
	methodDrain       = "drain"

	eventTaskState = "task.state"
)

// Error codes of the protocol. The codes "not_found", "at_capacity" and
// "internal" are in protocol.go.
const (
	codeInvalidSpec   = "invalid_spec"
	codeUnknownMethod = "unknown_method"
)

// WorkerBackendPrefix is the prefix of the name of a worker backend.
const WorkerBackendPrefix = "worker:"

// WorkerBackendName returns the name of the backend of one worker ID.
func WorkerBackendName(workerID string) string { return WorkerBackendPrefix + workerID }

// HelloV2 is sent by a worker when it connects.
type HelloV2 struct {
	WorkerID string            `json:"worker_id"`
	Name     string            `json:"name,omitempty"`
	Backend  string            `json:"backend"`
	Labels   map[string]string `json:"labels,omitempty"`
	// Capacity is the limit for tasks that are not exited. 0 means no limit.
	Capacity int    `json:"capacity"`
	Version  string `json:"version,omitempty"`
	// OneShot means that the hub assigns exactly one task.
	OneShot bool `json:"one_shot,omitempty"`
	// Tasks lists all tasks that the worker holds.
	Tasks []HelloTask `json:"tasks"`
	// ProtocolMinor is the minor version of the protocol that the worker
	// speaks. Additive features are gated on it and on Features.
	ProtocolMinor int `json:"protocol_minor,omitempty"`
	// Features lists optional features, for example "git_cache".
	Features []string `json:"features,omitempty"`
	// Facts describes the host of the worker.
	Facts *WorkerFacts `json:"facts,omitempty"`
	// Slots is the number of sessions that the worker runs at once. 0 means
	// that Capacity is the limit.
	Slots int `json:"slots,omitempty"`
	// Slot is the size of one slot.
	Slot *SlotSize `json:"slot,omitempty"`
	// Draining says that the worker takes no new task.
	Draining bool `json:"draining,omitempty"`
}

// ProtocolMinorV2 is the minor version of protocol v2 of this worker: 1 adds
// the challenge, facts, slots, task.list and drain.
const ProtocolMinorV2 = 1

// WorkerFacts describes the host of a worker.
type WorkerFacts struct {
	Arch               string   `json:"arch"`
	OS                 string   `json:"os,omitempty"`
	Kernel             string   `json:"kernel,omitempty"`
	VCPUs              int      `json:"vcpus"`
	MemoryMiB          int64    `json:"memory_mib,omitempty"`
	DiskTotalBytes     int64    `json:"disk_total_bytes,omitempty"`
	DiskFreeBytes      int64    `json:"disk_free_bytes,omitempty"`
	FirecrackerVersion string   `json:"firecracker_version,omitempty"`
	GuestKernel        string   `json:"guest_kernel,omitempty"`
	RuntimeVersion     string   `json:"runtime_version,omitempty"`
	KVM                bool     `json:"kvm,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
}

// SlotSize is the size of one slot of a worker.
type SlotSize struct {
	VCPUs      int `json:"vcpus"`
	MemoryMiB  int `json:"memory_mib"`
	ScratchGiB int `json:"scratch_gib,omitempty"`
}

// AuthChallenge is the first frame of the hub to an enrolled worker.
type AuthChallenge struct {
	// Nonce is 32 random bytes in base64url without padding.
	Nonce        string `json:"nonce"`
	ConnectionID string `json:"connection_id"`
	// Protocols lists the minor versions that the hub accepts.
	Protocols []int `json:"protocols,omitempty"`
}

// AuthResponse is the answer of the worker to an AuthChallenge.
type AuthResponse struct {
	WorkerID string `json:"worker_id"`
	// Alg is always "ed25519".
	Alg string `json:"alg"`
	// Sig is the Ed25519 signature of AuthMessage in base64url without padding.
	Sig string `json:"sig"`
}

// TaskListResult is the result of task.list.
type TaskListResult struct {
	Tasks []HelloTask `json:"tasks"`
}

// DrainParams are the params of drain.
type DrainParams struct {
	Draining bool `json:"draining"`
}

// HelloTask is one task of a hello message.
type HelloTask struct {
	TaskID string            `json:"task_id"`
	Status rtcore.TaskStatus `json:"status"`
}

type assignParams struct {
	Task rtcore.Task `json:"task"`
	// Worker holds extras for a microVM worker, for example
	// {"git_cache":[{"key","clone_url"}]}. A backend that implements
	// ExtrasBackend receives it.
	Worker json.RawMessage `json:"worker,omitempty"`
}

// ExtrasBackend is a task backend that takes the worker extras of
// task.assign. The worker calls StartWithExtras instead of Start when the
// backend implements it.
type ExtrasBackend interface {
	StartWithExtras(ctx context.Context, task rtcore.Task, extras json.RawMessage) (rtcore.TaskStatus, error)
}

type taskRef struct {
	TaskID string `json:"task_id"`
}

type cancelParams struct {
	TaskID       string `json:"task_id"`
	GraceSeconds int    `json:"grace_seconds"`
}

type statusBody struct {
	Status rtcore.TaskStatus `json:"status"`
}

// errorForV2 maps a backend error onto a wire error of version 2.
func errorForV2(err error) *rpcError {
	code := codeInternal
	switch {
	case errors.Is(err, rtcore.ErrNotFound):
		code = codeNotFound
	case errors.Is(err, rtcore.ErrInvalid):
		code = codeInvalidSpec
	case errors.Is(err, rtcore.ErrAtCapacity), errors.Is(err, ErrAtCapacity):
		code = codeAtCapacity
	case errors.Is(err, errUnknownMethod):
		code = codeUnknownMethod
	}
	return &rpcError{Code: code, Message: err.Error()}
}

// OneShotExitCode returns the process exit code of a one-shot worker for the
// final status of its task. The code is 0 only for the reason "completed".
func OneShotExitCode(st rtcore.TaskStatus) int {
	if st.State == rtcore.TaskExited && st.Reason == rtcore.SandboxCompleted {
		return 0
	}
	return 1
}

// OneShotError is returned by RunWorkerV2 in one-shot mode when the task did
// not end with the reason "completed".
type OneShotError struct {
	Status rtcore.TaskStatus
}

func (e *OneShotError) Error() string {
	return fmt.Sprintf("one-shot task %s ended with reason %q", e.Status.TaskID, e.Status.Reason)
}

// stateRank orders the states of a task. An older state never replaces a newer one.
func stateRank(s rtcore.TaskState) int {
	switch s {
	case rtcore.TaskAccepted:
		return 1
	case rtcore.TaskStarting:
		return 2
	case rtcore.TaskRunning:
		return 3
	case rtcore.TaskExited:
		return 4
	}
	return 0
}
