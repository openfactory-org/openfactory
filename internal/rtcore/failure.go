package rtcore

// FailureCode is the closed list of reasons why a run failed. The runtime
// reports only the codes below; the full list lives in the control plane.
type FailureCode string

const (
	// FailureEnvironmentSetupFailed means a clone, a setup command or an MCP server start failed.
	FailureEnvironmentSetupFailed FailureCode = "environment_setup_failed"
	// FailureHarnessAuthFailed means the credential of the harness is missing or refused.
	FailureHarnessAuthFailed FailureCode = "harness_auth_failed"
	// FailureAgentFailed means the harness ended the turn with an error.
	FailureAgentFailed FailureCode = "agent_failed"
)
