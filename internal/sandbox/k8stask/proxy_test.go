package k8stask

import (
	"reflect"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

func envOf(t *testing.T, spec map[string]any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range spec["containers"].([]any) {
		m := c.(map[string]any)
		if m["name"] != taskContainer {
			continue
		}
		for _, e := range m["env"].([]any) {
			kv := e.(map[string]any)
			out[kv["name"].(string)], _ = kv["value"].(string)
		}
	}
	return out
}

func TestProxyEnvNotSetWithoutRestriction(t *testing.T) {
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
		spec := podSpecOf(t, f, task.TaskID)
		if _, has := envOf(t, spec)["HTTPS_PROXY"]; has {
			t.Errorf("%s: proxy variables are set", name)
		}
		if init, _ := spec["initContainers"].([]any); len(init) != 0 {
			t.Errorf("%s: unexpected init containers %v", name, init)
		}
	}
}

func TestProxyEnvNotSetWithoutSetting(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := restrictedTask("run_c")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	if _, has := envOf(t, podSpecOf(t, f, task.TaskID))["HTTPS_PROXY"]; has {
		t.Error("proxy variables are set without a setting")
	}
}

func TestProxySidecar(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.EgressProxySidecarImage = "example.test/runner:1"
		o.RunnerImage = "example.test/runner:1"
	})
	task := restrictedTask("run_sc")
	task.NetworkPolicy.AllowedHosts = []string{"example.com", "git.example.com"}
	task.NetworkPolicy.DeniedHosts = []string{"bad.example.com"}
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	spec := podSpecOf(t, f, task.TaskID)
	init := spec["initContainers"].([]any)
	if len(init) != 3 {
		t.Fatalf("init containers = %v", init)
	}
	sc := init[0].(map[string]any)
	if sc["name"] != proxySidecarName || sc["restartPolicy"] != "Always" || sc["image"] != "example.test/runner:1" {
		t.Fatalf("sidecar = %v", sc)
	}
	want := []any{"openfactory", "netguard", "--listen", "127.0.0.1:3128",
		"--report-listen", "127.0.0.1:3129", "--allow", "example.com", "--allow", "git.example.com", "--deny", "bad.example.com"}
	if !reflect.DeepEqual(sc["command"], want) {
		t.Errorf("command = %v", sc["command"])
	}
	if init[1].(map[string]any)["name"] != egress.WaitName {
		t.Errorf("the wait container must follow the sidecar: %v", init[1])
	}
	if init[2].(map[string]any)["name"] != installName {
		t.Errorf("the runner copy container must follow the wait container: %v", init[2])
	}
	env := envOf(t, spec)
	if env["HTTPS_PROXY"] != "http://127.0.0.1:3128" || env["http_proxy"] != "http://127.0.0.1:3128" {
		t.Errorf("proxy env = %v", env)
	}
	if env[egress.ReportEnv] != "http://127.0.0.1:3129/blocked" {
		t.Errorf("report URL = %q", env[egress.ReportEnv])
	}
	if !strings.Contains(env["NO_PROXY"], "factory.internal") {
		t.Errorf("NO_PROXY = %q", env["NO_PROXY"])
	}

	np, ok := f.netpol(task.TaskID)
	if !ok {
		t.Fatal("no NetworkPolicy")
	}
	egress := dig(t, np, "spec", "egress").([]any)
	last := egress[len(egress)-1].(map[string]any)
	if _, hasTo := last["to"]; hasTo {
		t.Errorf("the sidecar rule must have no destination: %v", last)
	}
	ports := last["ports"].([]any)
	if len(ports) != 2 || ports[0].(map[string]any)["port"] != float64(80) && ports[0].(map[string]any)["port"] != 80 {
		t.Errorf("ports = %v", ports)
	}
}
