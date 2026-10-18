package core

import "testing"

func TestFailureTable(t *testing.T) {
	rows := []struct {
		code  FailureCode
		value string
		class FailureClass
		retry bool
	}{
		{FailureInvalidConfiguration, "invalid_configuration", FailureClassUser, false},
		{FailureEnvironmentSetupFailed, "environment_setup_failed", FailureClassUser, false},
		{FailureHarnessAuthFailed, "harness_auth_failed", FailureClassUser, false},
		{FailureExternalAuthenticationRequired, "external_authentication_required", FailureClassUser, false},
		{FailureIntegrationDisabled, "integration_disabled", FailureClassUser, false},
		{FailureAgentFailed, "agent_failed", FailureClassUser, false},
		{FailureOutOfMemory, "out_of_memory", FailureClassUser, false},
		{FailureResourceUnavailable, "resource_unavailable", FailureClassPlatform, true},
		{FailureSandboxCreateFailed, "sandbox_create_failed", FailureClassPlatform, true},
		{FailureNetworkPolicyTimeout, "network_policy_timeout", FailureClassPlatform, true},
		{FailureSandboxLost, "sandbox_lost", FailureClassPlatform, true},
		{FailureRunnerLost, "runner_lost", FailureClassPlatform, true},
		{FailureAgentProcessFailed, "agent_process_failed", FailureClassPlatform, false},
		{FailureInfrastructureTimeout, "infrastructure_timeout", FailureClassPlatform, false},
		{FailureInternalError, "internal_error", FailureClassPlatform, false},
		{FailureSpendLimitExceeded, "spend_limit_exceeded", FailureClassPlatform, false},
	}
	if len(rows) != 16 || len(AllFailureCodes()) != 16 {
		t.Fatalf("want 16 codes, table %d, all %d", len(rows), len(AllFailureCodes()))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		t.Run(r.value, func(t *testing.T) {
			if string(r.code) != r.value {
				t.Fatalf("value = %q, want %q", r.code, r.value)
			}
			if seen[r.value] {
				t.Fatalf("duplicate value %q", r.value)
			}
			seen[r.value] = true
			info, ok := LookupFailure(r.code)
			if !ok {
				t.Fatal("not found")
			}
			if info.Code != r.code || info.Class != r.class || info.Retry != r.retry {
				t.Fatalf("got %+v", info)
			}
			if !r.code.Valid() {
				t.Fatal("not valid")
			}
		})
	}
	if _, ok := LookupFailure("nonsense"); ok {
		t.Fatal("unknown code found")
	}
	if FailureCode("nonsense").Valid() {
		t.Fatal("unknown code valid")
	}
	for i, info := range AllFailureCodes() {
		if info.Code != rows[i].code {
			t.Fatalf("order differs at %d: %q", i, info.Code)
		}
	}
}

func TestRunnerFailureCode(t *testing.T) {
	tests := []struct {
		in   string
		want FailureCode
	}{
		{"environment_setup_failed", FailureEnvironmentSetupFailed},
		{"harness_auth_failed", FailureHarnessAuthFailed},
		{"agent_failed", FailureAgentFailed},
		{"", FailureAgentFailed},
		{"sandbox_lost", FailureAgentFailed},
		{"internal_error", FailureAgentFailed},
		{"nonsense", FailureAgentFailed},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := RunnerFailureCode(tt.in); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFailureForSandboxReason(t *testing.T) {
	tests := []struct {
		reason SandboxReason
		want   FailureCode
		ok     bool
	}{
		{SandboxCompleted, FailureAgentProcessFailed, true},
		{SandboxProcessFailed, FailureAgentProcessFailed, true},
		{SandboxImagePull, FailureSandboxCreateFailed, true},
		{SandboxUnschedulable, FailureSandboxCreateFailed, true},
		{SandboxNetworkPolicyTimeout, FailureNetworkPolicyTimeout, true},
		{SandboxInvalidSpec, FailureInvalidConfiguration, true},
		{SandboxContainerOOM, FailureOutOfMemory, true},
		{SandboxEvicted, FailureSandboxLost, true},
		{SandboxNodeLost, FailureSandboxLost, true},
		{SandboxDeadlineExceeded, FailureInfrastructureTimeout, true},
		{SandboxCancelled, "", false},
		{SandboxUnknown, FailureRunnerLost, true},
		{"weird", FailureRunnerLost, true},
	}
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			got, ok := FailureForSandboxReason(tt.reason)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("got (%q, %v), want (%q, %v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestAllFailureCodesCopy(t *testing.T) {
	a := AllFailureCodes()
	a[0].Code = "changed"
	a[0].Retry = true
	b := AllFailureCodes()
	if b[0].Code != FailureInvalidConfiguration || b[0].Retry {
		t.Fatalf("second call changed: %+v", b[0])
	}
}
