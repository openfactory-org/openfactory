package k8stask

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

func lockBackend(t *testing.T, f *fakeAPI, mod func(*Options)) *Backend {
	return newBackend(t, f, func(o *Options) {
		o.EgressProxySidecarImage = "example.test/runner:1"
		o.RunnerImage = "example.test/runner:1"
		o.EgressLock = egress.LockIPTables
		if mod != nil {
			mod(o)
		}
	})
}

func initNames(spec map[string]any) []string {
	var out []string
	for _, c := range spec["initContainers"].([]any) {
		out = append(out, c.(map[string]any)["name"].(string))
	}
	return out
}

func initByName(t *testing.T, spec map[string]any, name string) map[string]any {
	t.Helper()
	for _, c := range spec["initContainers"].([]any) {
		if m := c.(map[string]any); m["name"] == name {
			return m
		}
	}
	t.Fatalf("no init container %q in %v", name, initNames(spec))
	return nil
}

func TestEgressLockPodSpec(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, nil)
	task := restrictedTask("run_lock")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)

	// The lock runs first, then the proxy, then the wait for the
	// NetworkPolicy, then the runner copy.
	names := initNames(spec)
	if len(names) != 4 || names[0] != egress.LockName || names[1] != proxySidecarName ||
		names[2] != egress.WaitName || names[3] != installName {
		t.Fatalf("init containers = %v", names)
	}
	lock := initByName(t, spec, egress.LockName)
	if lock["restartPolicy"] != nil {
		t.Errorf("the lock must be a normal init container: %v", lock["restartPolicy"])
	}
	sc := lock["securityContext"].(map[string]any)
	caps := sc["capabilities"].(map[string]any)
	if got := caps["add"].([]any); len(got) != 2 || got[0] != "NET_ADMIN" || got[1] != "NET_RAW" {
		t.Errorf("capabilities add = %v", got)
	}
	if got := caps["drop"].([]any); len(got) != 1 || got[0] != "ALL" {
		t.Errorf("capabilities drop = %v", got)
	}
	if sc["runAsNonRoot"] != false || sc["allowPrivilegeEscalation"] != false {
		t.Errorf("security context = %v", sc)
	}
	script := lock["command"].([]any)[2].(string)
	for _, want := range []string{"--dports 80,443", "--uid-owner 65532", "REJECT --reject-with tcp-reset", "-o lo", "ip6tables"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}

	// The proxy runs as the proxy user, which differs from the task user.
	proxy := initByName(t, spec, proxySidecarName)
	if u := proxy["securityContext"].(map[string]any)["runAsUser"]; fmt.Sprint(u) != "65532" {
		t.Errorf("proxy user = %v", u)
	}
	if u := spec["securityContext"].(map[string]any)["runAsUser"]; fmt.Sprint(u) == "65532" {
		t.Errorf("task user equals the proxy user")
	}

	// The task container cannot gain privileges.
	for _, c := range spec["containers"].([]any) {
		m := c.(map[string]any)
		if m["name"] == taskContainer && m["securityContext"].(map[string]any)["allowPrivilegeEscalation"] != false {
			t.Errorf("the task container must not allow privilege escalation: %v", m["securityContext"])
		}
	}
}

func TestEgressLockOffAddsNothing(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, func(o *Options) { o.EgressLock = "none" })
	task := restrictedTask("run_nolock")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	for _, n := range initNames(spec) {
		if n == egress.LockName {
			t.Fatal("the lock is off, but the Pod has the lock container")
		}
	}
	proxy := initByName(t, spec, proxySidecarName)
	if u := proxy["securityContext"].(map[string]any)["runAsUser"]; fmt.Sprint(u) != "1000" {
		t.Errorf("without the lock, the proxy user stays the task user: %v", u)
	}
}

func TestEgressLockNotForUnrestrictedTask(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, nil)
	task := testTask("run_open", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	if init, _ := podSpecOf(t, f, task.TaskID)["initContainers"].([]any); len(init) != 1 {
		t.Errorf("an unrestricted task gets only the runner copy: %v", init)
	}
}

func TestEgressLockProxyUserDiffersFromTaskUser(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, func(o *Options) { o.RunAsUser = 65532 })
	task := restrictedTask("run_user")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	proxy := initByName(t, spec, proxySidecarName)
	if u := proxy["securityContext"].(map[string]any)["runAsUser"]; fmt.Sprint(u) != "65533" {
		t.Errorf("proxy user = %v, want 65533", u)
	}
	script := initByName(t, spec, egress.LockName)["command"].([]any)[2].(string)
	if !strings.Contains(script, "--uid-owner 65533") {
		t.Errorf("script = %s", script)
	}
}

func TestEgressLockControlOnPort443GoesThroughProxy(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, nil)
	task := restrictedTask("run_cp443") // control URL https://factory.internal
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	env := envOf(t, spec)
	if strings.Contains(env["NO_PROXY"], "factory.internal") {
		t.Errorf("NO_PROXY must not bypass the control plane: %q", env["NO_PROXY"])
	}
	cmd := initByName(t, spec, proxySidecarName)["command"].([]any)
	if !containsPair(cmd, "--always-allow", "factory.internal") {
		t.Errorf("the proxy must always allow the control plane: %v", cmd)
	}
}

func TestEgressLockControlOnOtherPortStaysDirect(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, nil)
	task := restrictedTask("run_cp8080")
	task.Env[core.TaskEnvControlURL] = "http://factory.internal:8080"
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	if !strings.Contains(envOf(t, spec)["NO_PROXY"], "factory.internal") {
		t.Errorf("NO_PROXY = %q", envOf(t, spec)["NO_PROXY"])
	}
	cmd := initByName(t, spec, proxySidecarName)["command"].([]any)
	for _, a := range cmd {
		if a == "--always-allow" {
			t.Errorf("unexpected --always-allow: %v", cmd)
		}
	}
}

func containsPair(cmd []any, flag, value string) bool {
	for i := 0; i+1 < len(cmd); i++ {
		if cmd[i] == flag && cmd[i+1] == value {
			return true
		}
	}
	return false
}

func TestEgressLockNeedsSidecarImage(t *testing.T) {
	f := newFakeAPI(t)
	_, err := New(Options{Config: startFake(t, f), EgressLock: "iptables"})
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	if _, err := New(Options{Config: startFake(t, f), EgressLock: "nftables", EgressProxySidecarImage: "x"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("an unknown lock gave %v", err)
	}
}

// --- enforcement check ---

func checkerResult(st egress.Enforcement, err error) *egress.Checker {
	return egress.NewChecker(func(context.Context) (egress.Enforcement, error) { return st, err }, nil)
}

func TestRestrictedTaskFailsWhenPoliciesAreNotEnforced(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.NetworkPolicyEnforcement = egress.EnforcementRequire
	})
	b.enforce = checkerResult(egress.NotEnforced, nil)
	_, err := b.Start(ctx5(t), restrictedTask("run_ne"))
	if !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "does not enforce NetworkPolicy") {
		t.Fatalf("err = %v", err)
	}
	if f.jobCount() != 0 {
		t.Error("a Job exists for a task that was refused")
	}
}

func TestRestrictedTaskStartsWhenPoliciesAreEnforced(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.NetworkPolicyEnforcement = egress.EnforcementRequire
	})
	b.enforce = checkerResult(egress.Enforced, nil)
	if _, err := b.Start(ctx5(t), restrictedTask("run_enf")); err != nil {
		t.Fatal(err)
	}
}

func TestRestrictedTaskStartsWithWarningInWarnMode(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	b.enforce = checkerResult(egress.NotEnforced, nil)
	b.enforce.Start()
	if _, err := b.enforce.Result(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Start(ctx5(t), restrictedTask("run_warn")); err != nil {
		t.Fatalf("warn mode must start the task: %v", err)
	}
}

func TestRequireModeFailsWhenTheCheckHasNoAnswer(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.NetworkPolicyEnforcement = egress.EnforcementRequire
	})
	b.enforce = checkerResult(egress.EnforcementUnknown, errors.New("image pull failed"))
	_, err := b.Start(ctx5(t), restrictedTask("run_unk"))
	if !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "image pull failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestAllowUnenforcedOverridesRequire(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.NetworkPolicyEnforcement = egress.EnforcementRequire
		o.AllowUnenforcedNetworkPolicy = true
	})
	b.enforce = checkerResult(egress.NotEnforced, nil)
	if _, err := b.Start(ctx5(t), restrictedTask("run_allow")); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestUnrestrictedTaskIgnoresTheCheck(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.NetworkPolicyEnforcement = egress.EnforcementRequire })
	b.enforce = checkerResult(egress.NotEnforced, nil)
	if _, err := b.Start(ctx5(t), testTask("run_free", 1)); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightStartsTheProbeInTheBackground(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.NetworkPolicyCheck = egress.CheckProbe
		o.NetworkPolicyProbeImage = "example.test/probe:1"
	})
	if b.enforce == nil {
		t.Fatal("the probe check is not set up")
	}
	started := make(chan struct{})
	b.enforce = egress.NewChecker(func(context.Context) (egress.Enforcement, error) {
		close(started)
		return egress.Enforced, nil
	}, nil)
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	<-started
	if got, err := b.enforce.Result(ctx5(t)); err != nil || got != egress.Enforced {
		t.Fatalf("result = %v, %v", got, err)
	}
	if b.NetworkPolicyEnforcement() != egress.Enforced {
		t.Errorf("NetworkPolicyEnforcement = %v", b.NetworkPolicyEnforcement())
	}
}

func TestProbeCheckNeedsAnImage(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.NetworkPolicyCheck = egress.CheckProbe })
	if b.enforce != nil {
		t.Error("no image is known, so no check can run")
	}
	b = newBackend(t, f, func(o *Options) {
		o.NetworkPolicyCheck = egress.CheckProbe
		o.RunnerImage = "example.test/runner:1"
	})
	if b.enforce == nil {
		t.Error("the runner image is the fallback image of the probe")
	}
}

func TestNetworkPolicySettingsAreValidated(t *testing.T) {
	f := newFakeAPI(t)
	for _, o := range []Options{
		{NetworkPolicyCheck: "sometimes"},
		{NetworkPolicyEnforcement: "maybe"},
	} {
		o.Config = startFake(t, f)
		if _, err := New(o); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("%+v: err = %v", o, err)
		}
	}
}

// The image user can be the proxy user, or root that can become it. With the
// lock, the backend refuses it.
func TestReviewLockWithImageUser(t *testing.T) {
	f := newFakeAPI(t)
	_, err := New(Options{Config: startFake(t, f), EgressLock: egress.LockIPTables,
		EgressProxySidecarImage: "example.test/runner:1", RunAsImageUser: true})
	if !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "numeric") {
		t.Errorf("RunAsImageUser with the lock: err = %v, want ErrInvalid", err)
	}

	b := lockBackend(t, f, nil)
	task := restrictedTask("run_imguser")
	task.Platform.User = "image"
	if _, err := b.Start(ctx5(t), task); !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), "numeric user") {
		t.Errorf("platform.linux.user=image with the lock: err = %v, want ErrInvalid", err)
	}
	f.mu.Lock()
	jobs, netpols := len(f.jobs), len(f.netpols)
	f.mu.Unlock()
	if jobs != 0 || netpols != 0 {
		t.Errorf("a refused task created %d Jobs and %d NetworkPolicies", jobs, netpols)
	}

	// An unrestricted task gets no lock, so the image user stays valid.
	open := testTask("run_imgopen", 1)
	open.Platform.User = "image"
	if _, err := b.Start(ctx5(t), open); err != nil {
		t.Errorf("unrestricted task with the image user: %v", err)
	}
}

func TestEgressLockHardensTaskContainer(t *testing.T) {
	f := newFakeAPI(t)
	b := lockBackend(t, f, nil)
	task := restrictedTask("run_harden")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	for _, c := range spec["containers"].([]any) {
		m := c.(map[string]any)
		if m["name"] != taskContainer {
			continue
		}
		sc := m["securityContext"].(map[string]any)
		if sc["runAsNonRoot"] != true {
			t.Errorf("runAsNonRoot = %v", sc["runAsNonRoot"])
		}
		if d := sc["capabilities"].(map[string]any)["drop"].([]any); len(d) != 1 || d[0] != "ALL" {
			t.Errorf("capabilities drop = %v", d)
		}
	}
	if u := spec["securityContext"].(map[string]any)["runAsUser"]; fmt.Sprint(u) == fmt.Sprint(egress.DefaultProxyUser) {
		t.Errorf("task user equals the proxy user")
	}
}

// The pod template can set the user of the Pod or of the sandbox container.
// The proxy user must differ from it.
func TestEgressLockProxyUserAvoidsTemplateUsers(t *testing.T) {
	for _, tc := range []struct{ name, tmpl, want string }{
		{"pod", "securityContext: {runAsUser: 65532}\n", "65533"},
		{"container", "containers:\n- name: sandbox\n  securityContext: {runAsUser: 65532}\n", "65533"},
		{"both", "securityContext: {runAsUser: 65533}\ncontainers:\n- name: sandbox\n  securityContext: {runAsUser: 65532}\n", "65534"},
		{"other user", "securityContext: {runAsUser: 2000}\n", "65532"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			b := lockBackend(t, f, func(o *Options) { o.PodTemplate = tc.tmpl })
			task := restrictedTask("run_tmpluser")
			if _, err := b.Start(ctx5(t), task); err != nil {
				t.Fatal(err)
			}
			spec := podSpecOf(t, f, task.TaskID)
			if u := initByName(t, spec, proxySidecarName)["securityContext"].(map[string]any)["runAsUser"]; fmt.Sprint(u) != tc.want {
				t.Errorf("proxy user = %v, want %s", u, tc.want)
			}
			script := initByName(t, spec, egress.LockName)["command"].([]any)[2].(string)
			if !strings.Contains(script, "--uid-owner "+tc.want) {
				t.Errorf("script = %s", script)
			}
		})
	}
}
