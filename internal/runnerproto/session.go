package runnerproto

import (
	"errors"
	"fmt"
	"time"
)

// StartRequest is the body of the session start request.
type StartRequest struct {
	// InstanceID is a random value that the runner makes at process start.
	InstanceID string `json:"instance_id"`
	// RunnerVersion is the version of the runner.
	RunnerVersion string `json:"runner_version"`
	// SandboxID is the ID of the sandbox or job.
	SandboxID string `json:"sandbox_id"`
	// Platform is the platform of the sandbox.
	Platform Platform `json:"platform"`
}

// Platform names the operating system and the architecture of the sandbox.
type Platform struct {
	// OS is the operating system, for example linux.
	OS string `json:"os"`
	// Arch is the architecture, for example amd64.
	Arch string `json:"arch"`
}

// Validate checks that the required fields are set.
func (r StartRequest) Validate() error {
	switch {
	case r.InstanceID == "":
		return errors.New("instance_id is empty")
	case r.RunnerVersion == "":
		return errors.New("runner_version is empty")
	case r.Platform.OS == "":
		return errors.New("platform.os is empty")
	case r.Platform.Arch == "":
		return errors.New("platform.arch is empty")
	}
	return nil
}

// StartResponse is the body of the response to the session start request.
type StartResponse struct {
	// SessionToken is the token for all later requests.
	SessionToken string `json:"session_token"`
	// SessionTokenExpiresAt is the time when the session token expires.
	SessionTokenExpiresAt time.Time `json:"session_token_expires_at"`
	// RunID is the ID of the run.
	RunID string `json:"run_id"`
	// Session is the number of the session of the run.
	Session int `json:"session"`
	// ConversationID is the ID of the conversation of the run.
	ConversationID string `json:"conversation_id"`
	// Workspace describes the workspace to prepare.
	Workspace Workspace `json:"workspace"`
	// Harness describes the harness to start.
	Harness Harness `json:"harness"`
	// Env is the environment for the processes of the runner.
	Env map[string]string `json:"env"`
	// IdleTimeoutSeconds is the idle time after which the session ends.
	IdleTimeoutSeconds int `json:"idle_timeout_seconds"`
	// IdleTimeoutSet is true when IdleTimeoutSeconds is a decision of the
	// server. Then 0 means "end the session when the turn ends". A server
	// that omits it (an older server) gets the runner default for 0.
	IdleTimeoutSet bool `json:"idle_timeout_set,omitempty"`
	// Limits are the limits of the session.
	Limits Limits `json:"limits"`
	// Cloud is the cloud identity of the session. Nil means
	// that the factory configures no cloud provider.
	Cloud *CloudIdentity `json:"cloud,omitempty"`
	// RunnerVersion is the runner version that the control plane expects.
	// When it is set and differs from the version of the runner, the runner
	// stops. Empty means that the control plane does not check.
	RunnerVersion string `json:"runner_version,omitempty"`
	// NetworkPolicy is the network policy of the session. The in-sandbox
	// lock enforces it. Nil means that the control plane sends no policy: a
	// runner that must lock the network then fails closed.
	NetworkPolicy *NetworkPolicy `json:"network_policy,omitempty"`
	// EgressLock is EgressLockRequired or EgressLockOff. A runner that must
	// lock the network fails closed when it is EgressLockOff.
	EgressLock string `json:"egress_lock,omitempty"`
}

// Values of StartResponse.EgressLock.
const (
	// EgressLockRequired means that the runner locks the network when the
	// provider asks for it (OF_EGRESS_LOCK).
	EgressLockRequired = "required"
	// EgressLockOff means that the control plane does not want the lock.
	EgressLockOff = "off"
)

// NetworkPolicy is the network policy of a session.
type NetworkPolicy struct {
	// Restricted is true when only the allowed hosts are reachable.
	Restricted bool `json:"restricted"`
	// AllowedHosts are the hosts that a restricted policy allows.
	AllowedHosts []string `json:"allowed_hosts"`
	// DeniedHosts are hosts that are never reachable, also when allowed.
	DeniedHosts []string `json:"denied_hosts"`
	// AlwaysAllow are platform hosts that are always reachable: the control
	// plane and the run data store. A leading "." matches subdomains.
	AlwaysAllow []string `json:"always_allow,omitempty"`
}

// CloudIdentity tells the runner which cloud credential files to write. The
// files hold no token: the SDKs get each token from the runner.
type CloudIdentity struct {
	GCP *CloudGCP `json:"gcp,omitempty"`
}

// CloudGCP is the GCP part of a CloudIdentity.
type CloudGCP struct {
	Audience            string `json:"audience"`
	ProjectNumber       string `json:"project_number"`
	ServiceAccountEmail string `json:"service_account_email,omitempty"`
}

// Workspace describes the workspace of a session.
type Workspace struct {
	// Repositories are the repositories to clone.
	Repositories []Repository `json:"repositories"`
	// SetupCommands are the commands to run after the clone.
	SetupCommands []SetupCommand `json:"setup_commands"`
	// SetupTimeoutSeconds is the time limit for all setup commands.
	SetupTimeoutSeconds int `json:"setup_timeout_seconds"`
	// Skills are the skills to write to the workspace.
	Skills []Skill `json:"skills"`
	// Snapshot says if the runner restores and saves snapshots.
	Snapshot SnapshotConfig `json:"snapshot"`
}

// Repository is one repository of the workspace.
type Repository struct {
	// Forge is the kind of forge, for example GITHUB.
	Forge string `json:"forge"`
	// CloneURL is the clone URL, without credentials.
	CloneURL string `json:"clone_url"`
	// Path is the directory of the clone in the workspace.
	Path string `json:"path"`
	// Ref is the branch, tag or commit to check out.
	Ref string `json:"ref"`
}

// SetupCommand is one setup command.
type SetupCommand struct {
	// Origin tells where the command comes from: "machine" or "environment".
	Origin string `json:"origin"`
	// Command is the shell command.
	Command string `json:"command"`
}

// Skill is a skill that the runner writes to the workspace.
type Skill struct {
	// Name is the name of the skill.
	Name string `json:"name"`
	// Files maps a file path to its content.
	Files map[string]string `json:"files"`
}

// SnapshotConfig says how the runner uses snapshots.
type SnapshotConfig struct {
	// Restore says that the runner restores the newest snapshot.
	Restore bool `json:"restore"`
	// Enabled says that the runner saves a snapshot at the end.
	Enabled bool `json:"enabled"`
	// SkipWorkspace says that the runner does not save the workspace part.
	// It is true when the agent sets "snapshot: false". The harness_state
	// part is saved when Enabled is true, also when SkipWorkspace is true.
	SkipWorkspace bool `json:"skip_workspace,omitempty"`
	// MaxPartBytes limits the size of one part (OF_SNAPSHOT_MAX_BYTES). 0 means no limit.
	MaxPartBytes int64 `json:"max_part_bytes,omitempty"`
}

// Harness describes the harness of the session.
type Harness struct {
	// Type is the harness type, for example claude.
	Type string `json:"type"`
	// Model is the model name.
	Model string `json:"model"`
	// ReasoningLevel is the reasoning level of the model.
	ReasoningLevel string `json:"reasoning_level"`
	// SystemPrompt is the system prompt.
	SystemPrompt string `json:"system_prompt"`
	// ResumeSessionID is the ID of the harness session to resume.
	ResumeSessionID string `json:"resume_session_id"`
	// MCPServers maps a server name to its configuration.
	MCPServers map[string]MCPServer `json:"mcp_servers"`
}

// MCPServer is the configuration of one MCP server.
type MCPServer struct {
	// URL is the URL of a remote server.
	URL string `json:"url,omitempty"`
	// Auth names the authentication method, for example session_token.
	Auth string `json:"auth,omitempty"`
	// Command is the command of a local server.
	Command string `json:"command,omitempty"`
	// Args are the arguments of the command.
	Args []string `json:"args,omitempty"`
	// Env is the environment of the command.
	Env map[string]string `json:"env,omitempty"`
}

// Limits are the limits of a session.
type Limits struct {
	// MaxSessionSeconds is the longest duration of the session.
	MaxSessionSeconds int `json:"max_session_seconds"`
	// EventBatchBytes is the largest size of an event batch.
	EventBatchBytes int `json:"event_batch_bytes"`
}

// SecretsResponse is the body of the response to the secrets request.
type SecretsResponse struct {
	// Env are secret environment variables.
	Env map[string]string `json:"env"`
	// Files are secret files to write.
	Files []SecretFile `json:"files"`
	// HarnessAuth are the credentials of the harness.
	HarnessAuth map[string]string `json:"harness_auth"`
	// HarnessAuthDefault tells that HarnessAuth is the default model
	// credential (the definition sets no harness.auth). The runner then
	// drops HarnessAuth when its own environment already holds a model
	// credential for the harness.
	HarnessAuthDefault bool `json:"harness_auth_default,omitempty"`
	// MCPHeaders maps an MCP server name to its request headers.
	MCPHeaders map[string]map[string]string `json:"mcp_headers"`
	// MCPEnv maps the name of an MCP server with a command to its
	// environment. The values can hold secrets.
	MCPEnv map[string]map[string]string `json:"mcp_env,omitempty"`
	// Redact lists the strings that the runner removes from events.
	Redact []string `json:"redact"`
}

// SecretFile is a secret file to write in the sandbox.
type SecretFile struct {
	// Path is the path of the file.
	Path string `json:"path"`
	// Mode is the file mode as an octal string, for example 0600.
	Mode string `json:"mode"`
	// ContentBase64 is the content of the file in base64.
	ContentBase64 string `json:"content_base64"`
}

// EndReason is the reason in a session end request.
type EndReason string

// The reasons for a session end.
const (
	// EndIdle means that the idle timer ended the session.
	EndIdle EndReason = "idle"
	// EndStopped means that the session was stopped.
	EndStopped EndReason = "stopped"
	// EndCancelled means that the run was cancelled.
	EndCancelled EndReason = "cancelled"
	// EndSetupFailed means that the setup failed.
	EndSetupFailed EndReason = "setup_failed"
	// EndDeadline means that the session reached its time limit.
	EndDeadline EndReason = "deadline"
	// EndFatal means that the runner had a fatal error.
	EndFatal EndReason = "fatal"
)

// Valid reports whether r is a known end reason.
func (r EndReason) Valid() bool {
	switch r {
	case EndIdle, EndStopped, EndCancelled, EndSetupFailed, EndDeadline, EndFatal:
		return true
	}
	return false
}

// EndRequest is the body of the session end request.
type EndRequest struct {
	// Reason is the reason for the end.
	Reason EndReason `json:"reason"`
	// LastEventSeq is the sequence number of the last event of the session.
	LastEventSeq int64 `json:"last_event_seq"`
	// FailureCode is set for setup_failed and fatal.
	FailureCode string `json:"failure_code,omitempty"`
	// Message is a text for people.
	Message string `json:"message,omitempty"`
	// Exit is the exit information of the harness.
	Exit *ExitInfo `json:"exit,omitempty"`
}

// ExitInfo is the exit information of a process.
type ExitInfo struct {
	// Code is the exit code.
	Code int `json:"code"`
}

// Validate checks the rules of an end request.
func (r EndRequest) Validate() error {
	if !r.Reason.Valid() {
		return fmt.Errorf("invalid reason %q", r.Reason)
	}
	if r.LastEventSeq < 0 {
		return errors.New("last_event_seq is negative")
	}
	if (r.Reason == EndSetupFailed || r.Reason == EndFatal) && r.FailureCode == "" {
		return fmt.Errorf("failure_code is empty for reason %q", r.Reason)
	}
	return nil
}

// TokenResponse is the body of the response to the session token request.
type TokenResponse struct {
	// SessionToken is the new session token.
	SessionToken string `json:"session_token"`
	// SessionTokenExpiresAt is the time when the new token expires.
	SessionTokenExpiresAt time.Time `json:"session_token_expires_at"`
}
