package sdk

import "github.com/openfactory-org/openfactory/internal/core"

// The types below are aliases of the server's own domain types, so a plugin
// in another module can implement Forge, Responder or SandboxBackend and
// build events without importing internal/ packages. Values flow between the
// SDK and the host without conversion.
type (
	// Forge is a code forge (clone URLs, pull requests, comments, statuses).
	Forge = core.Forge
	// PullRequestInput is the input of Forge.CreatePullRequest.
	PullRequestInput = core.PullRequestInput
	// PullRequestRef identifies a pull request.
	PullRequestRef = core.PullRequestRef
	// CommitStatus is the input of Forge.SetCommitStatus.
	CommitStatus = core.CommitStatus
	// ReviewInput is the input of Forge.SubmitReview.
	ReviewInput = core.ReviewInput
	// ReviewComment is one inline comment of a ReviewInput.
	ReviewComment = core.ReviewComment

	// ReviewThread is a review thread that ReviewContextReader returns.
	ReviewThread = core.ReviewThread
	// ThreadComment is one comment of a ReviewThread.
	ThreadComment = core.ThreadComment
	// FileDiff is the diff of one file that ReviewContextReader returns.
	FileDiff = core.FileDiff

	// Responder posts outbound messages back to a work source.
	Responder = core.Responder
	// OutboundMessage is something a factory says back to a work source.
	OutboundMessage = core.OutboundMessage
	// IssueTracker is the optional write-back of a tracker Responder:
	// set the state of an issue and attach a pull request.
	IssueTracker = core.IssueTracker

	// Event is an inbound event; Ingress handlers emit them with Emit or Bus.
	// Set ActionKey to the same value on all events that one provider action
	// causes, for example the events of one webhook delivery. The engine then
	// routes the action one time. When ActionKey is empty, the engine makes it
	// from DeliveryID (without a ":<event type>" suffix) and the provider.
	Event = core.Event
	// Actor is who caused an Event.
	Actor = core.Actor
	// SourceRef identifies where a work item came from.
	SourceRef = core.SourceRef
	// EventBus is the publish/subscribe interface returned by Bus.
	EventBus = core.EventBus

	// SandboxBackend starts one sandbox for one session of a run and reports
	// its state. Implement it and set Plugin.SandboxBackend.
	SandboxBackend = core.TaskBackend
	// SandboxSpec is one session of one run, as a SandboxBackend receives it.
	SandboxSpec = core.Task
	// ToolImage is an image that holds a harness CLI.
	ToolImage = core.TaskImage
	// SandboxPlatform is the platform that the sandbox must have.
	SandboxPlatform = core.TaskPlatform
	// SandboxResources are the resources of the sandbox.
	SandboxResources = core.TaskResources
	// SandboxNetworkPolicy is the network policy of the sandbox.
	SandboxNetworkPolicy = core.TaskNetworkPolicy
	// SandboxStatus is the state of a sandbox that a SandboxBackend reports.
	SandboxStatus = core.TaskStatus
	// SandboxState is the state of a sandbox: accepted, starting, running or exited.
	SandboxState = core.TaskState
	// SandboxReason is the end reason of a sandbox.
	SandboxReason = core.SandboxReason

	// Team is a tenant.
	Team = core.Team
	// Integration is a team's integration record (provider config and
	// secret references).
	Integration = core.Integration
	// Factory is a factory record.
	Factory = core.Factory
	// RepoRef is a repository of a factory definition.
	RepoRef = core.RepoRef
)

// TopicInboundEvent is the Bus topic Ingress handlers publish events on.
const TopicInboundEvent = core.TopicInboundEvent

// Sentinel errors understood by the host (mapped to gRPC status codes).
var (
	ErrNotFound     = core.ErrNotFound
	ErrConflict     = core.ErrConflict
	ErrInvalid      = core.ErrInvalid
	ErrUnauthorized = core.ErrUnauthorized
	ErrForbidden    = core.ErrForbidden
	// ErrOperationNotSupported maps to UNIMPLEMENTED: the forge or the
	// tracker does not support the operation.
	ErrOperationNotSupported = core.ErrOperationNotSupported
)

// States and end reasons of a sandbox.
const (
	SandboxAccepted = core.TaskAccepted
	SandboxStarting = core.TaskStarting
	SandboxRunning  = core.TaskRunning
	SandboxExited   = core.TaskExited

	SandboxCompleted     = core.SandboxCompleted
	SandboxProcessFailed = core.SandboxProcessFailed
	SandboxImagePull     = core.SandboxImagePull
	SandboxUnschedulable = core.SandboxUnschedulable
	SandboxInvalidSpec   = core.SandboxInvalidSpec
	SandboxCancelled     = core.SandboxCancelled
	SandboxUnknown       = core.SandboxUnknown
)

// ErrAtCapacity is returned by SandboxBackend.Start when the backend has no free
// slot. The host maps it to the gRPC code RESOURCE_EXHAUSTED.
var ErrAtCapacity = core.ErrAtCapacity
