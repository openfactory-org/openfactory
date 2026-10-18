package core

// FailureCode is the closed list of reasons why a run failed.
type FailureCode string

// FailureClass tells whose fault a failure is.
type FailureClass string

const (
	// FailureClassUser means the cause is in the configuration or the code of the user.
	FailureClassUser FailureClass = "user"
	// FailureClassPlatform means the cause is in the platform.
	FailureClassPlatform FailureClass = "platform"
)

// FailureInfo describes one failure code.
type FailureInfo struct {
	Code  FailureCode
	Class FailureClass
	Retry bool
}

// MaxRetrySessions is the number of sessions in sequence that can end with a retry code.
const MaxRetrySessions = 3

const (
	// FailureInvalidConfiguration means a secret, machine or MCP server is missing at the start.
	FailureInvalidConfiguration FailureCode = "invalid_configuration"
	// FailureEnvironmentSetupFailed means a clone, a setup command or an MCP server start failed.
	FailureEnvironmentSetupFailed FailureCode = "environment_setup_failed"
	// FailureHarnessAuthFailed means the credential of the harness is missing or refused.
	FailureHarnessAuthFailed FailureCode = "harness_auth_failed"
	// FailureExternalAuthenticationRequired means the run_as user has no linked account.
	FailureExternalAuthenticationRequired FailureCode = "external_authentication_required"
	// FailureIntegrationDisabled means the integration that the run needs is not active.
	FailureIntegrationDisabled FailureCode = "integration_disabled"
	// FailureAgentFailed means the harness ended the turn with an error.
	FailureAgentFailed FailureCode = "agent_failed"
	// FailureOutOfMemory means the task container passed its memory limit.
	FailureOutOfMemory FailureCode = "out_of_memory"
	// FailureResourceUnavailable means the backend did not accept the task because of a transient error.
	FailureResourceUnavailable FailureCode = "resource_unavailable"
	// FailureSandboxCreateFailed means the sandbox could not be created or did not start in time.
	FailureSandboxCreateFailed FailureCode = "sandbox_create_failed"
	// FailureNetworkPolicyTimeout means the network policy of a restricted task did
	// not take effect in time. The task command never ran, so nothing ran without the limit.
	FailureNetworkPolicyTimeout FailureCode = "network_policy_timeout"
	// FailureSandboxLost means the sandbox was evicted, its node was lost, or the backend does not know it.
	FailureSandboxLost FailureCode = "sandbox_lost"
	// FailureRunnerLost means the runner stopped its requests and the backend has no end reason.
	FailureRunnerLost FailureCode = "runner_lost"
	// FailureAgentProcessFailed means the runner process ended without a session end.
	FailureAgentProcessFailed FailureCode = "agent_process_failed"
	// FailureInfrastructureTimeout means the queue timeout or the maximum session time passed.
	FailureInfrastructureTimeout FailureCode = "infrastructure_timeout"
	// FailureInternalError means a fault in the control plane.
	FailureInternalError FailureCode = "internal_error"
	// FailureSpendLimitExceeded means the spend of the factory or the team
	// passed its limit at a turn end, and the control plane stopped the run tree.
	FailureSpendLimitExceeded FailureCode = "spend_limit_exceeded"
)

// AllFailureCodes returns the failure codes in table order. It returns a new slice on each call.
func AllFailureCodes() []FailureInfo {
	return []FailureInfo{
		{FailureInvalidConfiguration, FailureClassUser, false},
		{FailureEnvironmentSetupFailed, FailureClassUser, false},
		{FailureHarnessAuthFailed, FailureClassUser, false},
		{FailureExternalAuthenticationRequired, FailureClassUser, false},
		{FailureIntegrationDisabled, FailureClassUser, false},
		{FailureAgentFailed, FailureClassUser, false},
		{FailureOutOfMemory, FailureClassUser, false},
		{FailureResourceUnavailable, FailureClassPlatform, true},
		{FailureSandboxCreateFailed, FailureClassPlatform, true},
		{FailureNetworkPolicyTimeout, FailureClassPlatform, true},
		{FailureSandboxLost, FailureClassPlatform, true},
		{FailureRunnerLost, FailureClassPlatform, true},
		{FailureAgentProcessFailed, FailureClassPlatform, false},
		{FailureInfrastructureTimeout, FailureClassPlatform, false},
		{FailureInternalError, FailureClassPlatform, false},
		{FailureSpendLimitExceeded, FailureClassPlatform, false},
	}
}

// LookupFailure returns the description of a failure code and whether the code is known.
func LookupFailure(code FailureCode) (FailureInfo, bool) {
	for _, info := range AllFailureCodes() {
		if info.Code == code {
			return info, true
		}
	}
	return FailureInfo{}, false
}

// Valid reports whether the code is in the closed list.
func (c FailureCode) Valid() bool {
	_, ok := LookupFailure(c)
	return ok
}

// RunnerFailureCode returns the failure code for a code that a runner reported.
// A runner can report only environment_setup_failed, harness_auth_failed and
// agent_failed. Every other string gives agent_failed.
func RunnerFailureCode(code string) FailureCode {
	switch c := FailureCode(code); c {
	case FailureEnvironmentSetupFailed, FailureHarnessAuthFailed, FailureAgentFailed:
		return c
	}
	return FailureAgentFailed
}

// FailureForSandboxReason maps the end reason of a sandbox to the failure code
// of the run, for the case where the runner did not end the session. The
// reason cancelled has no failure code, and the result is false.
func FailureForSandboxReason(reason SandboxReason) (FailureCode, bool) {
	switch reason {
	case SandboxCompleted, SandboxProcessFailed:
		return FailureAgentProcessFailed, true
	case SandboxImagePull, SandboxUnschedulable:
		return FailureSandboxCreateFailed, true
	case SandboxNetworkPolicyTimeout:
		return FailureNetworkPolicyTimeout, true
	case SandboxInvalidSpec:
		return FailureInvalidConfiguration, true
	case SandboxContainerOOM:
		return FailureOutOfMemory, true
	case SandboxEvicted, SandboxNodeLost:
		return FailureSandboxLost, true
	case SandboxDeadlineExceeded:
		return FailureInfrastructureTimeout, true
	case SandboxCancelled:
		return "", false
	}
	return FailureRunnerLost, true
}

// FailureCodeError is an error that carries the failure code of a run.
type FailureCodeError struct {
	Code FailureCode
	Err  error
}

func (e *FailureCodeError) Error() string {
	if e.Err == nil {
		return string(e.Code)
	}
	return e.Err.Error()
}

func (e *FailureCodeError) Unwrap() error { return e.Err }
