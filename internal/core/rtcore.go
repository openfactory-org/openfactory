package core

import "github.com/openfactory-org/openfactory/internal/rtcore"

// The runtime types live in internal/rtcore. These aliases keep the control
// plane building until it is deleted.

// Harness.
type (
	Harness        = rtcore.Harness
	HarnessRequest = rtcore.HarnessRequest
	HarnessEvent   = rtcore.HarnessEvent
	HarnessType    = rtcore.HarnessType
	HarnessConfig  = rtcore.HarnessConfig
	HarnessAuth    = rtcore.HarnessAuth
	Usage          = rtcore.Usage
	Skill          = rtcore.Skill
	MCPServer      = rtcore.MCPServer
)

const (
	HarnessClaude = rtcore.HarnessClaude
	HarnessCodex  = rtcore.HarnessCodex
	UsageKnown    = rtcore.UsageKnown
	UsagePartial  = rtcore.UsagePartial
)

// Sandbox processes.
type (
	Sandbox     = rtcore.Sandbox
	Process     = rtcore.Process
	ExecRequest = rtcore.ExecRequest
	ExecResult  = rtcore.ExecResult
)

// Tasks.
type (
	Task              = rtcore.Task
	TaskStatus        = rtcore.TaskStatus
	TaskState         = rtcore.TaskState
	SandboxReason     = rtcore.SandboxReason
	TaskBackend       = rtcore.TaskBackend
	TaskPlatform      = rtcore.TaskPlatform
	TaskResources     = rtcore.TaskResources
	TaskNetworkPolicy = rtcore.TaskNetworkPolicy
	TaskImage         = rtcore.TaskImage //nolint:staticcheck // kept until the control plane is deleted
)

const (
	TaskAccepted = rtcore.TaskAccepted
	TaskStarting = rtcore.TaskStarting
	TaskRunning  = rtcore.TaskRunning
	TaskExited   = rtcore.TaskExited

	SandboxCompleted            = rtcore.SandboxCompleted
	SandboxProcessFailed        = rtcore.SandboxProcessFailed
	SandboxImagePull            = rtcore.SandboxImagePull
	SandboxUnschedulable        = rtcore.SandboxUnschedulable
	SandboxNetworkPolicyTimeout = rtcore.SandboxNetworkPolicyTimeout
	SandboxInvalidSpec          = rtcore.SandboxInvalidSpec
	SandboxContainerOOM         = rtcore.SandboxContainerOOM
	SandboxEvicted              = rtcore.SandboxEvicted
	SandboxNodeLost             = rtcore.SandboxNodeLost
	SandboxDeadlineExceeded     = rtcore.SandboxDeadlineExceeded
	SandboxCancelled            = rtcore.SandboxCancelled
	SandboxUnknown              = rtcore.SandboxUnknown

	TaskEnvControlURL     = rtcore.TaskEnvControlURL
	TaskEnvRunID          = rtcore.TaskEnvRunID
	TaskEnvSession        = rtcore.TaskEnvSession
	TaskEnvBootstrapToken = rtcore.TaskEnvBootstrapToken
)

var (
	// ErrAtCapacity is returned by TaskBackend.Start when the backend has no free slot.
	ErrAtCapacity = rtcore.ErrAtCapacity
	// ErrUnavailable is returned by a task backend that cannot answer now.
	ErrUnavailable = rtcore.ErrUnavailable
)

// TaskID returns the ID of the task for one session of one run.
func TaskID(runID string, session int) string { return rtcore.TaskID(runID, session) }

// ParseTaskID splits a task ID at the last dot into a run ID and a session number.
func ParseTaskID(taskID string) (runID string, session int, err error) {
	return rtcore.ParseTaskID(taskID)
}

// Network policy.
type EffectiveNetworkPolicy = rtcore.EffectiveNetworkPolicy

// Secrets.
type SecretType = rtcore.SecretType

const (
	SecretAnthropicKey   = rtcore.SecretAnthropicKey
	SecretAnthropicOAuth = rtcore.SecretAnthropicOAuth
	SecretOpenAIKey      = rtcore.SecretOpenAIKey
)

// Run data kept outside the database.
const (
	ArtifactsDir    = rtcore.ArtifactsDir
	EnvArtifactsDir = rtcore.EnvArtifactsDir
)

// NewID returns a random, URL-safe identifier with the given prefix, e.g. NewID("run") => "run_k3j...".
func NewID(prefix string) string { return rtcore.NewID(prefix) }
