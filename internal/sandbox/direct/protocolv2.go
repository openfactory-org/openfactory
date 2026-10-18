package direct

import (
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
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
const SubprotocolV2 = "openfactory.worker.v2"

// Methods and events of version 2.
const (
	methodTaskAssign  = "task.assign"
	methodTaskCancel  = "task.cancel"
	methodTaskStatus  = "task.status"
	methodTaskDestroy = "task.destroy"

	eventTaskState = "task.state"
)

// Error codes of the protocol. The codes "not_found", "at_capacity" and
// "internal" are in protocol.go.
const (
	codeInvalidSpec = "invalid_spec"
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
}

// HelloTask is one task of a hello message.
type HelloTask struct {
	TaskID string          `json:"task_id"`
	Status core.TaskStatus `json:"status"`
}

type assignParams struct {
	Task core.Task `json:"task"`
}

type taskRef struct {
	TaskID string `json:"task_id"`
}

type cancelParams struct {
	TaskID       string `json:"task_id"`
	GraceSeconds int    `json:"grace_seconds"`
}

type statusBody struct {
	Status core.TaskStatus `json:"status"`
}

// toErrorV2 maps a wire error of version 2 onto sentinel errors.
func (e *rpcError) toErrorV2() error {
	switch e.Code {
	case codeNotFound:
		return fmt.Errorf("%s: %w", e.Message, core.ErrNotFound)
	case codeInvalidSpec:
		return fmt.Errorf("%s: %w", e.Message, core.ErrInvalid)
	case codeAtCapacity:
		return fmt.Errorf("%s: %w", e.Message, core.ErrAtCapacity)
	default:
		return errors.New(e.Message)
	}
}

// errorForV2 maps a backend error onto a wire error of version 2.
func errorForV2(err error) *rpcError {
	code := codeInternal
	switch {
	case errors.Is(err, core.ErrNotFound):
		code = codeNotFound
	case errors.Is(err, core.ErrInvalid):
		code = codeInvalidSpec
	case errors.Is(err, core.ErrAtCapacity), errors.Is(err, ErrAtCapacity):
		code = codeAtCapacity
	}
	return &rpcError{Code: code, Message: err.Error()}
}

// OneShotExitCode returns the process exit code of a one-shot worker for the
// final status of its task. The code is 0 only for the reason "completed".
func OneShotExitCode(st core.TaskStatus) int {
	if st.State == core.TaskExited && st.Reason == core.SandboxCompleted {
		return 0
	}
	return 1
}

// OneShotError is returned by RunWorkerV2 in one-shot mode when the task did
// not end with the reason "completed".
type OneShotError struct {
	Status core.TaskStatus
}

func (e *OneShotError) Error() string {
	return fmt.Sprintf("one-shot task %s ended with reason %q", e.Status.TaskID, e.Status.Reason)
}

// stateRank orders the states of a task. An older state never replaces a newer one.
func stateRank(s core.TaskState) int {
	switch s {
	case core.TaskAccepted:
		return 1
	case core.TaskStarting:
		return 2
	case core.TaskRunning:
		return 3
	case core.TaskExited:
		return 4
	}
	return 0
}
