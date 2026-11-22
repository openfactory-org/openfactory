package rtcore

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TaskState is the state of a task in a backend.
type TaskState string

const (
	// TaskAccepted means the backend stored the task durably. The sandbox does not exist yet.
	TaskAccepted TaskState = "accepted"
	// TaskStarting means the backend creates the sandbox: schedule, image pull, start.
	TaskStarting TaskState = "starting"
	// TaskRunning means the main process of the task is in operation.
	TaskRunning TaskState = "running"
	// TaskExited means the sandbox ended.
	TaskExited TaskState = "exited"
)

// SandboxReason is the end reason of a sandbox.
type SandboxReason string

const (
	// SandboxCompleted means the main process ended with exit code 0.
	SandboxCompleted SandboxReason = "completed"
	// SandboxProcessFailed means the main process ended with a different exit code.
	SandboxProcessFailed SandboxReason = "process_failed"
	// SandboxImagePull means the image pull failed.
	SandboxImagePull SandboxReason = "image_pull"
	// SandboxUnschedulable means the backend could not schedule the sandbox.
	SandboxUnschedulable SandboxReason = "unschedulable"
	// SandboxNetworkPolicyTimeout means the network policy of a restricted task
	// did not take effect before the wait of the backend ended. The task
	// command never ran.
	SandboxNetworkPolicyTimeout SandboxReason = "network_policy_timeout"
	// SandboxInvalidSpec means the backend refused the sandbox spec as invalid.
	SandboxInvalidSpec SandboxReason = "invalid_spec"
	// SandboxContainerOOM means the container passed its memory limit.
	SandboxContainerOOM SandboxReason = "container_oom"
	// SandboxEvicted means the platform evicted the sandbox.
	SandboxEvicted SandboxReason = "evicted"
	// SandboxNodeLost means the node of the sandbox was lost.
	SandboxNodeLost SandboxReason = "node_lost"
	// SandboxDeadlineExceeded means the sandbox passed its deadline.
	SandboxDeadlineExceeded SandboxReason = "deadline_exceeded"
	// SandboxCancelled means the task was cancelled.
	SandboxCancelled SandboxReason = "cancelled"
	// SandboxUnknown means the backend does not know why the sandbox ended.
	SandboxUnknown SandboxReason = "unknown"
)

// Names of the four variables that every task carries.
const (
	// TaskEnvControlURL is the variable that holds the URL of the control plane.
	TaskEnvControlURL = "OF_CONTROL_URL"
	// TaskEnvRunID is the variable that holds the run ID.
	TaskEnvRunID = "OF_RUN_ID"
	// TaskEnvSession is the variable that holds the session number.
	TaskEnvSession = "OF_SESSION"
	// TaskEnvBootstrapToken is the variable that holds the bootstrap token.
	TaskEnvBootstrapToken = "OF_BOOTSTRAP_TOKEN" //nolint:gosec // G101: an environment variable name, not a credential
)

// TaskImage is an image that holds a harness CLI.
//
// Deprecated: the sandbox image carries the harness CLIs. TaskImage,
// Task.RunnerImage and Task.ToolImages stay only until the Go control plane
// is deleted; the runtime ignores them.
type TaskImage struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// TaskPlatform is the platform that the sandbox must have.
type TaskPlatform struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
	// User is a numeric user ID, or "image" for the user of the image. Empty
	// means the default of the backend (1000).
	User string `json:"user,omitempty"`
}

// TaskResources are the resources of the sandbox.
type TaskResources struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// TaskNetworkPolicy is the network policy of the sandbox.
type TaskNetworkPolicy struct {
	Mode         string   `json:"mode"`
	AllowedHosts []string `json:"allowed_hosts"`
	DeniedHosts  []string `json:"denied_hosts"`
}

// Task is one session of one run, as a backend receives it.
type Task struct {
	TaskID                  string             `json:"task_id"`
	RunID                   string             `json:"run_id"`
	Session                 int                `json:"session"`
	TeamID                  string             `json:"team_id"`
	FactoryID               string             `json:"factory_id,omitempty"`
	Image                   string             `json:"image"`
	RunnerImage             string             `json:"runner_image,omitempty"`
	ToolImages              []TaskImage        `json:"tool_images,omitempty"`
	Platform                TaskPlatform       `json:"platform"`
	Resources               TaskResources      `json:"resources"`
	Env                     map[string]string  `json:"env"`
	NetworkPolicy           *TaskNetworkPolicy `json:"network_policy,omitempty"`
	MaxSessionSeconds       int                `json:"max_session_seconds"`
	FailureRetentionSeconds int                `json:"failure_retention_seconds"`
	WorkspaceSizeLimit      string             `json:"workspace_size_limit,omitempty"`
	Labels                  map[string]string  `json:"labels,omitempty"`
}

// TaskStatus is the state of a task that a backend reports.
type TaskStatus struct {
	TaskID    string        `json:"task_id"`
	State     TaskState     `json:"state"`
	Reason    SandboxReason `json:"reason,omitempty"`
	ExitCode  *int          `json:"exit_code,omitempty"`
	Message   string        `json:"message,omitempty"`
	SandboxID string        `json:"sandbox_id,omitempty"`
	StartedAt *time.Time    `json:"started_at,omitempty"`
	EndedAt   *time.Time    `json:"ended_at,omitempty"`
}

// ErrAtCapacity is returned by TaskBackend.Start when the backend has no free slot.
var ErrAtCapacity = errors.New("backend at capacity")

// ErrUnavailable is returned by a task backend that cannot answer now, for
// example the backend of a remote worker that is not connected. It does not
// mean that the task is gone.
var ErrUnavailable = errors.New("backend unavailable")

// TaskBackend starts one sandbox for one task and reports its state.
type TaskBackend interface {
	// Name returns the name of the backend.
	Name() string
	// Start stores the task and returns when the task is accepted.
	// A second Start with the same TaskID returns the existing task.
	Start(ctx context.Context, task Task) (TaskStatus, error)
	// Status returns ErrNotFound for a task that the backend does not know.
	Status(ctx context.Context, taskID string) (TaskStatus, error)
	// Watch sends a status on each change until the task exited.
	Watch(ctx context.Context, taskID string) (<-chan TaskStatus, error)
	// Cancel asks the task to stop. After graceSeconds, the backend stops it.
	Cancel(ctx context.Context, taskID string, graceSeconds int) error
	// Destroy removes the sandbox and its resources. It is safe to call two times.
	Destroy(ctx context.Context, taskID string) error
	// List returns the tasks that the backend holds, for the orphan sweep.
	List(ctx context.Context) ([]TaskStatus, error)
	// Preflight checks that the backend can start tasks.
	Preflight(ctx context.Context) error
}

// TaskID returns the ID of the task for one session of one run.
func TaskID(runID string, session int) string {
	return runID + "." + strconv.Itoa(session)
}

// ParseTaskID splits a task ID at the last dot into a run ID and a session number.
func ParseTaskID(taskID string) (runID string, session int, err error) {
	i := strings.LastIndex(taskID, ".")
	if i < 0 {
		return "", 0, fmt.Errorf("task id %q has no dot", taskID)
	}
	runID = taskID[:i]
	if runID == "" {
		return "", 0, fmt.Errorf("task id %q has an empty run id", taskID)
	}
	n, convErr := strconv.Atoi(taskID[i+1:])
	if convErr != nil || n < 1 {
		return "", 0, fmt.Errorf("task id %q has an invalid session number", taskID)
	}
	return runID, n, nil
}

// Validate returns the first problem of the task as an error that names the field.
func (t Task) Validate() error {
	if t.RunID == "" {
		return errors.New("run_id is empty")
	}
	if t.Session < 1 {
		return fmt.Errorf("session must be 1 or more, got %d", t.Session)
	}
	if want := TaskID(t.RunID, t.Session); t.TaskID != want {
		return fmt.Errorf("task_id is %q, want %q", t.TaskID, want)
	}
	if t.TeamID == "" {
		return errors.New("team_id is empty")
	}
	if t.Image == "" {
		return errors.New("image is empty")
	}
	if t.Platform.OS != "linux" {
		return fmt.Errorf("platform.os must be linux, got %q", t.Platform.OS)
	}
	if t.Platform.Arch != "amd64" && t.Platform.Arch != "arm64" {
		return fmt.Errorf("platform.arch must be amd64 or arm64, got %q", t.Platform.Arch)
	}
	for _, name := range []string{TaskEnvControlURL, TaskEnvRunID, TaskEnvSession, TaskEnvBootstrapToken} {
		if t.Env[name] == "" {
			return fmt.Errorf("env.%s is empty", name)
		}
	}
	if t.Env[TaskEnvRunID] != t.RunID {
		return fmt.Errorf("env.%s does not equal run_id", TaskEnvRunID)
	}
	if t.Env[TaskEnvSession] != strconv.Itoa(t.Session) {
		return fmt.Errorf("env.%s does not equal session", TaskEnvSession)
	}
	if t.MaxSessionSeconds <= 0 {
		return fmt.Errorf("max_session_seconds must be more than 0, got %d", t.MaxSessionSeconds)
	}
	if t.FailureRetentionSeconds < 0 {
		return fmt.Errorf("failure_retention_seconds must be 0 or more, got %d", t.FailureRetentionSeconds)
	}
	return nil
}

// Redacted returns a deep copy of the task in which the bootstrap token is
// replaced by "***". The receiver does not change. Use the copy for logs.
func (t Task) Redacted() Task {
	c := t
	if t.ToolImages != nil {
		c.ToolImages = append([]TaskImage(nil), t.ToolImages...)
	}
	if t.Env != nil {
		c.Env = make(map[string]string, len(t.Env))
		for k, v := range t.Env {
			c.Env[k] = v
		}
		if _, ok := c.Env[TaskEnvBootstrapToken]; ok {
			c.Env[TaskEnvBootstrapToken] = "***"
		}
	}
	if t.Labels != nil {
		c.Labels = make(map[string]string, len(t.Labels))
		for k, v := range t.Labels {
			c.Labels[k] = v
		}
	}
	if t.NetworkPolicy != nil {
		np := *t.NetworkPolicy
		if np.AllowedHosts != nil {
			np.AllowedHosts = append([]string(nil), np.AllowedHosts...)
		}
		if np.DeniedHosts != nil {
			np.DeniedHosts = append([]string(nil), np.DeniedHosts...)
		}
		c.NetworkPolicy = &np
	}
	return c
}

// Exited reports whether the task reached the state exited.
func (s TaskStatus) Exited() bool {
	return s.State == TaskExited
}
