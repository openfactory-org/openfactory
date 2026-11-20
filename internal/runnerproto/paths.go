package runnerproto

const (
	// Version is the version of the runner protocol.
	Version = "runner/v1"
	// HeaderRunnerVersion is the request header with the version of the runner.
	HeaderRunnerVersion = "OF-Runner-Version"
	// PathPrefix is the start of each path of the protocol.
	PathPrefix = "/runner/v1"
	// PathSessionStart is the path of the session start request.
	PathSessionStart = "/runner/v1/session/start"
	// PathSessionSecrets is the path of the secrets request.
	PathSessionSecrets = "/runner/v1/session/secrets" //nolint:gosec // G101: a URL path, not a credential
	// PathSessionEvents is the path of the events request.
	PathSessionEvents = "/runner/v1/session/events"
	// PathSessionCommands is the path of the commands request.
	PathSessionCommands = "/runner/v1/session/commands"
	// PathSessionEnd is the path of the session end request.
	PathSessionEnd = "/runner/v1/session/end"
	// PathSessionUploads is the path of the upload request.
	PathSessionUploads = "/runner/v1/session/uploads"
	// PathSessionDownloads is the path of the download request.
	PathSessionDownloads = "/runner/v1/session/downloads"
	// PathGitCredential is the path of the git credential request.
	PathGitCredential = "/runner/v1/session/git-credential" //nolint:gosec // G101: a URL path, not a credential
	// PathCloudToken is the path of the cloud token request.
	PathCloudToken = "/runner/v1/session/cloud-token" //nolint:gosec // G101: a URL path, not a credential
	// PathSessionToken is the path of the session token renewal request.
	PathSessionToken = "/runner/v1/session/token" //nolint:gosec // G101: a URL path, not a credential
	// PathConversation is the path of the conversation request.
	PathConversation = "/runner/v1/session/conversation"
)

// Limits of the protocol.
const (
	// MaxCommandWaitSeconds is the longest wait of a commands request.
	MaxCommandWaitSeconds = 25
	// MaxEventContentBytes is the longest content of one event.
	MaxEventContentBytes = 256 * 1024
	// MaxSetupOutputBytes is the longest output of a setup step event.
	MaxSetupOutputBytes = 16 * 1024
	// DefaultEventBatchBytes is the default size limit of an event batch.
	DefaultEventBatchBytes = 1 << 20
	// RunnerSilenceSeconds is the time without a request after which the
	// control plane asks the backend for the task state.
	RunnerSilenceSeconds = 90
)

// Error codes in a response body.
const (
	// CodeSessionSuperseded means that a newer session exists for the run.
	CodeSessionSuperseded = "session_superseded"
	// CodeRunCancelled means that the run was cancelled.
	CodeRunCancelled = "run_cancelled"
	// CodeConfigurationError means that the configuration of the run is not
	// valid, for example a repository without a code-forge integration. The
	// control plane already ended the session and failed the run. The status
	// is 422, and a retry gives the same result.
	CodeConfigurationError = "configuration_error"
)

// ErrorBody is the JSON body of an error response.
type ErrorBody struct {
	// Code is the error code.
	Code string `json:"code"`
	// Message is a text for people.
	Message string `json:"message,omitempty"`
	// Ack is the newest sequence number that the control plane has.
	Ack *int64 `json:"ack,omitempty"`
}
