package k8stask

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

// The wait keeps the task container back until the NetworkPolicy of the Pod is
// active (package egress, wait.go).

func waitOf(t *testing.T, f *fakeAPI, task core.Task) map[string]any {
	t.Helper()
	return initByName(t, podSpecOf(t, f, task.TaskID), egress.WaitName)
}

func hasInit(spec map[string]any, name string) bool {
	init, _ := spec["initContainers"].([]any)
	for _, c := range init {
		if m, _ := c.(map[string]any); m["name"] == name {
			return true
		}
	}
	return false
}

func TestEgressWaitForRestrictedTask(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.EgressWaitImage = "example.test/wait:1"
	})
	task := restrictedTask("run_w")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	w := initByName(t, spec, egress.WaitName)

	// The command waits for a failed dial of the kubelet of the node, with a
	// time that is limited.
	want := []any{"openfactory", "netprobe", "wait-blocked", "--host", "$(OF_EGRESS_WAIT_HOST)", "--port", "10250", "--timeout", "30s"}
	if !reflect.DeepEqual(w["command"], want) {
		t.Errorf("command = %v, want %v", w["command"], want)
	}
	if w["image"] != "example.test/wait:1" {
		t.Errorf("image = %v", w["image"])
	}
	env := w["env"].([]any)[0].(map[string]any)
	if env["name"] != egress.WaitHostEnv ||
		dig(t, env, "valueFrom", "fieldRef", "fieldPath") != "status.hostIP" {
		t.Errorf("env = %v", env)
	}
	if w["restartPolicy"] != nil {
		t.Errorf("the wait must be a normal init container, so that Kubernetes starts the next one only after it ended: %v", w["restartPolicy"])
	}
	if w["terminationMessagePolicy"] != "FallbackToLogsOnError" {
		t.Errorf("the status of the task needs the log of a failed wait: %v", w["terminationMessagePolicy"])
	}
	sc := w["securityContext"].(map[string]any)
	if sc["allowPrivilegeEscalation"] != false || sc["readOnlyRootFilesystem"] != true {
		t.Errorf("security context = %v", sc)
	}
	if got := sc["capabilities"].(map[string]any)["drop"].([]any); len(got) != 1 || got[0] != "ALL" {
		t.Errorf("capabilities drop = %v", got)
	}
	if egress.WaitExitCode != 75 {
		t.Errorf("exit code %d: the status mapping and the docs say 75", egress.WaitExitCode)
	}

	// Only the wait comes before the task: it is the first init container
	// without a sidecar, and it runs before any container with task code.
	if names := initNames(spec); names[0] != egress.WaitName {
		t.Errorf("init containers = %v", names)
	}
}

func TestEgressWaitNotForUnrestrictedTask(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.EgressProxySidecarImage = "example.test/runner:1"
	})
	for name, task := range map[string]core.Task{
		"no policy": testTask("run_a", 1),
		"unrestricted": func() core.Task {
			x := testTask("run_b", 1)
			x.NetworkPolicy = &core.TaskNetworkPolicy{Mode: "unrestricted"}
			return x
		}(),
	} {
		if _, err := b.Start(ctx5(t), task); err != nil {
			t.Fatal(err)
		}
		if hasInit(podSpecOf(t, f, task.TaskID), egress.WaitName) {
			t.Errorf("%s: an unrestricted task must not wait", name)
		}
	}
}

// An unknown mode counts as restricted (fail closed), so it waits too.
func TestEgressWaitForUnknownMode(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.EgressWaitImage = "example.test/wait:1" })
	task := restrictedTask("run_u")
	task.NetworkPolicy.Mode = "something-new"
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	if !hasInit(podSpecOf(t, f, task.TaskID), egress.WaitName) {
		t.Error("a task with an unknown mode must wait")
	}
}

func TestEgressWaitOrderWithSidecarLockAndImages(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, nil)
	task := restrictedTask("run_o")
	task.ToolImages = []core.TaskImage{{Name: "claude", Image: "example.test/tool:1"}}
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	// The lock and the proxy are first. Then the wait. Everything that runs an
	// image of the task (the copy of the runner and of the tools) follows it.
	want := []string{egress.LockName, proxySidecarName, egress.WaitName, installName, "tool-claude"}
	if got := initNames(podSpecOf(t, f, task.TaskID)); !reflect.DeepEqual(got, want) {
		t.Errorf("init containers = %v, want %v", got, want)
	}
}

func TestEgressWaitImageChoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Options)
		want string
	}{
		{"sandbox image", func(*Options) {}, "example.test/build:1"},
		{"sidecar image", func(o *Options) { o.EgressProxySidecarImage = "example.test/sidecar:1" }, "example.test/sidecar:1"},
		{"runner image", func(o *Options) {
			o.EgressProxySidecarImage = "example.test/sidecar:1"
			o.RunnerImage = "example.test/runner:1"
		}, "example.test/runner:1"},
		{"wait image", func(o *Options) {
			o.RunnerImage = "example.test/runner:1"
			o.EgressWaitImage = "example.test/wait:1"
		}, "example.test/wait:1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			b := newBackend(t, f, tc.mod)
			task := restrictedTask("run_i")
			if _, err := b.Start(ctx5(t), task); err != nil {
				t.Fatal(err)
			}
			if got := waitOf(t, f, task)["image"]; got != tc.want {
				t.Errorf("image = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEgressWaitTimeoutAndTarget(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.EgressWaitTimeout = 45 * time.Second
		o.EgressWaitAddr = "[fd00::5]:9000"
	})
	task := restrictedTask("run_t")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	w := waitOf(t, f, task)
	want := []any{"openfactory", "netprobe", "wait-blocked", "--addr", "[fd00::5]:9000", "--timeout", "45s"}
	if !reflect.DeepEqual(w["command"], want) {
		t.Errorf("command = %v, want %v", w["command"], want)
	}
	if w["env"] != nil {
		t.Errorf("a fixed target needs no node address: %v", w["env"])
	}
}

func TestEgressWaitOffSwitches(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*Options)
	}{
		{"negative timeout", func(o *Options) { o.EgressWaitTimeout = -1 }},
		// The policy cannot be created or is not enforced: it would never take
		// effect, and the wait would always fail.
		{"unenforced policy allowed", func(o *Options) { o.AllowUnenforcedNetworkPolicy = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			b := newBackend(t, f, tc.mod)
			task := restrictedTask("run_off")
			if _, err := b.Start(ctx5(t), task); err != nil {
				t.Fatal(err)
			}
			if hasInit(podSpecOf(t, f, task.TaskID), egress.WaitName) {
				t.Error("the wait must be off")
			}
		})
	}
}

func TestEgressWaitSkippedWhenClusterDoesNotEnforce(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.EgressProxySidecarImage = "example.test/runner:1"
		o.NetworkPolicyCheck = egress.CheckProbe
		o.NetworkPolicyEnforcement = egress.EnforcementWarn
	})
	b.enforce = checkerResult(egress.NotEnforced, nil)
	b.enforce.Start()
	if _, err := b.enforce.Result(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	task := restrictedTask("run_ne")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	if hasInit(podSpecOf(t, f, task.TaskID), egress.WaitName) {
		t.Error("the policy never takes effect on this cluster, so the wait would always fail")
	}
}

func TestEgressWaitTargetIsValidated(t *testing.T) {
	for _, bad := range []string{"10.0.0.1", "host:", ":80", "host:0", "host:99999", "host:http"} {
		if err := egress.ValidateWaitAddr(bad); err == nil {
			t.Errorf("%q is accepted", bad)
		}
	}
	for _, good := range []string{"", "10.0.0.1:10250", "[fd00::1]:80", "kubelet.local:10250"} {
		if err := egress.ValidateWaitAddr(good); err != nil {
			t.Errorf("%q: %v", good, err)
		}
	}
	f := newFakeAPI(t)
	if _, err := New(Options{Config: startFake(t, f), EgressWaitAddr: "nope"}); err == nil {
		t.Error("New accepts a bad wait target")
	}
}

// A failed wait is reported with the end reason network_policy_timeout, and
// the run gets the failure code network_policy_timeout, which the platform retries.
func TestEgressWaitFailureCode(t *testing.T) {
	code, ok := core.FailureForSandboxReason(core.SandboxNetworkPolicyTimeout)
	if !ok || code != core.FailureNetworkPolicyTimeout || string(code) != "network_policy_timeout" {
		t.Fatalf("code = %q, %v", code, ok)
	}
	if info, _ := core.LookupFailure(code); info.Class != core.FailureClassPlatform || !info.Retry {
		t.Errorf("info = %+v", info)
	}
	if msg := egress.WaitMessage("10.0.0.1:10250", 30*time.Second); !strings.Contains(msg, "10.0.0.1:10250") || !strings.Contains(msg, "30s") {
		t.Errorf("message = %q", msg)
	}
}
