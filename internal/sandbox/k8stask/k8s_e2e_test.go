//go:build e2e_k8s

package k8stask

// End-to-end tests of the task backend on a real Kubernetes cluster that you choose. They
// need no Helm and no model call. Run them on any cluster where you
// may create a namespace:
//
//	OF_E2E_KUBE_CONTEXT=<context> go test -tags e2e_k8s -run '^TestK8s' -v -count=1 ./internal/sandbox/k8stask/
//	(or: make e2e-k8s OF_E2E_KUBE_CONTEXT=<context>)
//
// Variables:
//
//	OF_E2E_KUBE_CONTEXT   context of the kubeconfig. The tests skip if it is
//	                      not set. OF_E2E_KUBECONFIG gives the file (default:
//	                      the usual kubeconfig).
//	OF_E2E_RUNNER_IMAGE   image of the runner: the server image
//	                      (ghcr.io/openfactory-org/openfactory:<tag>). Test 1
//	                      uses it as the runner and as the task image.
//	                      The cluster must be able to pull it. Test 1 skips
//	                      if it is not set.
//	OF_E2E_TOOL_IMAGE     a tool image, for TestK8sToolImages (for example
//	                      ghcr.io/openfactory-org/tool-claude:<tag>). The test
//	                      skips if it is not set. OF_E2E_TOOL_COMMAND is the
//	                      program of the tool image that the test runs
//	                      (default claude). OF_E2E_TASK_IMAGE is the task image
//	                      (default ubuntu:22.04): it must not hold the tool.
//	OF_E2E_PULL_POLICY    image pull policy of the task Pods. Default
//	                      IfNotPresent. Use Never for a local image.
//	OF_E2E_ARCH           platform arch of the task (amd64 or arm64). Default:
//	                      the arch of the machine that runs the test.
//	OF_E2E_CA_FILE        CA file of the cluster, for the server startup test
//	                      when the API address is not local.
//	OF_E2E_SERVER_BIN     path of an openfactory binary for the startup test.
//	                      Default: the test builds ./cmd/openfactory.
//	OF_E2E_KEEP           set to 1 to keep the test namespaces.
//
// Each test makes its own namespace and deletes it at the end.
//
// TestK8sDeadline        a task whose deadline passes fails with the code
//                        infrastructure_timeout, and Kubernetes removes its
//                        Pod and its Job.
// TestK8sToolImages      a tool image is copied into a Pod that runs as a
//                        user that is not root, and its bin directory is on
//                        the PATH of the task. It needs OF_E2E_TOOL_IMAGE.
// TestK8sRoleWithoutJobs a server whose Role has no rule for Jobs stops at
//                        startup with an error that names the rule.
//
// The four tests TestK8sNet* check the network limits of a task. They need
// OF_E2E_PROXY_IMAGE and run on any cluster. They find out by a measurement
// whether the cluster enforces NetworkPolicy, and check the right thing. The
// code is in net_e2e_test.go. Variables:
//
//	OF_E2E_PROXY_IMAGE    the server image (it holds openfactory and iptables).
//	                      The tests skip if it is not set.
//	OF_E2E_CLIENT_IMAGE   image of the task container. It needs sh and curl.
//	                      The runner image works. Default: curlimages/curl:8.10.1
//	OF_E2E_NGINX_IMAGE    image of the web servers that the tasks call.
//	OF_E2E_WEB_COMMAND    shell command that serves HTTP on port 80 in that
//	                      image, if it is not a web server. Example for the
//	                      sandbox image: python3 -m http.server 80
//	OF_E2E_NETPOL_ENFORCED  true or false: what you know about the cluster.
//
// TestK8sNetEnforcementProbe  the probe gives the verdict that the test
//                        measured, and OF_K8S_NETWORK_POLICY_ENFORCEMENT
//                        "require" refuses and "warn" allows a restricted task.
// TestK8sNetEgressProxy  the proxy allows a host and refuses another one.
// TestK8sNetEgressLock   OF_K8S_EGRESS_LOCK=iptables stops a program that
//                        ignores the proxy variables.
// TestK8sNetPolicyBlocksDirectEgress  NetworkPolicy blocks direct egress, also
//                        for the very first command of the task. It skips if
//                        the cluster does not enforce NetworkPolicy.
// TestK8sNetPolicyWaitTimeout  a NetworkPolicy that does not take effect in
//                        time ends the task with network_policy_timeout, and
//                        the task command never runs.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
)

// k8sEnv is one test namespace on the cluster.
type k8sEnv struct {
	t   *testing.T
	cfg *kubernetes.RestConfig
	ns  string
	// api is a backend that gives access to the API with the credentials of the
	// person who runs the test.
	api *Backend
}

func newK8sEnv(t *testing.T, name string) *k8sEnv {
	t.Helper()
	kctx := os.Getenv("OF_E2E_KUBE_CONTEXT")
	if kctx == "" {
		t.Skip("set OF_E2E_KUBE_CONTEXT to run the tests on a real cluster")
	}
	cfg, err := kubernetes.LoadConfig(os.Getenv("OF_E2E_KUBECONFIG"), kctx)
	if err != nil {
		t.Fatalf("load the kubeconfig (context %q): %v", kctx, err)
	}
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	e := &k8sEnv{t: t, cfg: cfg, ns: "of-e2e-" + name + "-" + hex.EncodeToString(rnd[:])}
	e.api, err = New(Options{Config: cfg, Namespace: e.ns})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.call(context.Background(), http.MethodPost, "/api/v1/namespaces", map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": e.ns},
	}, nil); err != nil {
		t.Fatalf("create the namespace %s: %v", e.ns, err)
	}
	t.Logf("namespace %s on context %s", e.ns, kctx)
	t.Cleanup(func() {
		if os.Getenv("OF_E2E_KEEP") == "1" {
			t.Logf("namespace %s stays (OF_E2E_KEEP=1)", e.ns)
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := e.call(cctx, http.MethodDelete, "/api/v1/namespaces/"+e.ns, nil, nil); err != nil && !errors.Is(err, core.ErrNotFound) {
			t.Errorf("delete the namespace %s: %v", e.ns, err)
		}
	})
	return e
}

func (e *k8sEnv) call(ctx context.Context, method, path string, body, out any) error {
	return e.api.do(ctx, method, path, "", body, out)
}

func (e *k8sEnv) path(kind string) string {
	return "/api/v1/namespaces/" + url.PathEscape(e.ns) + "/" + kind
}

func (e *k8sEnv) waitServiceAccount(name string) {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		err := e.call(context.Background(), http.MethodGet, e.path("serviceaccounts/"+name), nil, nil)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the namespace has no service account %s: %v", name, err)
		}
		time.Sleep(time.Second)
	}
}

// TestK8sDeadline starts a task Job with the real runner, against a control
// plane that does not answer, and with a deadline of a few seconds. The runner
// waits for the control plane, so the task cannot end before the deadline. The
// test then checks the status that the backend reports, the failure code of
// the run, and that Kubernetes removes the Pod (at the deadline) and the Job
// (after its time to live) with no call of Destroy.
func TestK8sDeadline(t *testing.T) {
	image := os.Getenv("OF_E2E_RUNNER_IMAGE")
	if image == "" {
		if os.Getenv("OF_E2E_KUBE_CONTEXT") != "" {
			t.Skip("set OF_E2E_RUNNER_IMAGE (the runner image that the cluster can pull)")
		}
	}
	e := newK8sEnv(t, "deadline")
	e.waitServiceAccount("default")
	pull := os.Getenv("OF_E2E_PULL_POLICY")
	if pull == "" {
		pull = "IfNotPresent"
	}
	var logs bytes.Buffer
	b, err := New(Options{
		Config: e.cfg, Namespace: e.ns, ImagePullPolicy: pull,
		RunnerImage:  image,
		StopGrace:    5 * time.Second,
		PollInterval: time.Second,
		Logger:       slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// A control URL that nothing serves: a port on a Service without Pods.
	// The runner retries the start of the session for 2 minutes.
	svc := map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": "no-control-plane", "namespace": e.ns},
		"spec": map[string]any{
			"selector": map[string]any{"app": "nothing"},
			"ports":    []any{map[string]any{"port": 8080, "targetPort": 8080}},
		},
	}
	if err := e.call(context.Background(), http.MethodPost, e.path("services"), svc, nil); err != nil {
		t.Fatalf("create the service: %v", err)
	}

	const deadlineSeconds, grace = 3, 5
	runID := "run-e2e-deadline-" + hex.EncodeToString(randBytes(3))
	task := core.Task{
		TaskID: core.TaskID(runID, 1), RunID: runID, Session: 1, TeamID: "team-e2e",
		Image: image, RunnerImage: image,
		Platform: core.TaskPlatform{OS: "linux", Arch: runtimeArch()},
		Env: map[string]string{
			core.TaskEnvControlURL:     "http://no-control-plane." + e.ns + ".svc:8080",
			core.TaskEnvRunID:          runID,
			core.TaskEnvSession:        "1",
			core.TaskEnvBootstrapToken: "e2e-token",
		},
		// The deadline of the Job is this value plus the grace time.
		MaxSessionSeconds:       deadlineSeconds,
		FailureRetentionSeconds: 20,
		NetworkPolicy:           &core.TaskNetworkPolicy{Mode: "unrestricted"},
	}
	ctx := context.Background()
	started := time.Now()
	if _, err := b.Start(ctx, task); err != nil {
		t.Fatalf("start: %v", err)
	}
	jobDeadline := deadlineSeconds + grace

	// Wait for the Pod to run, so that the test proves that the deadline ended
	// a Pod that was alive.
	var podName string
	waitFor(t, 3*time.Minute, "the task Pod runs", func() (bool, string) {
		st, err := b.Status(ctx, task.TaskID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		if st.Exited() && st.Reason != core.SandboxDeadlineExceeded {
			t.Fatalf("the task ended before the deadline: reason %q: %s", st.Reason, st.Message)
		}
		podName = st.SandboxID
		return st.State == core.TaskRunning || st.Exited(), fmt.Sprintf("state %s", st.State)
	})
	t.Logf("Pod %s runs %s after the start", podName, time.Since(started).Round(time.Second))

	var final core.TaskStatus
	waitFor(t, time.Duration(jobDeadline)*time.Second+3*time.Minute, "the task exits", func() (bool, string) {
		st, err := b.Status(ctx, task.TaskID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		final = st
		return st.Exited(), fmt.Sprintf("state %s", st.State)
	})
	t.Logf("task ended after %s: state=%s reason=%s message=%q", time.Since(started).Round(time.Second), final.State, final.Reason, final.Message)

	if final.Reason != core.SandboxDeadlineExceeded {
		t.Fatalf("reason = %q, want %q (message %q)", final.Reason, core.SandboxDeadlineExceeded, final.Message)
	}
	if !strings.Contains(strings.ToLower(final.Message), "deadline") {
		t.Errorf("message = %q, want a message that names the deadline", final.Message)
	}
	code, ok := core.FailureForSandboxReason(final.Reason)
	if !ok || code != core.FailureInfrastructureTimeout {
		t.Errorf("failure code = %q (ok %v), want %q", code, ok, core.FailureInfrastructureTimeout)
	}
	t.Logf("failure code of the run: %s", code)
	if elapsed := time.Since(started); elapsed < time.Duration(deadlineSeconds)*time.Second {
		t.Errorf("the task ended after %s, before its deadline of %ds", elapsed, deadlineSeconds)
	}

	// Kubernetes removes the Pod when the Job fails at its deadline.
	waitFor(t, 2*time.Minute, "Kubernetes removes the Pod", func() (bool, string) {
		pods, err := b.listPods(ctx, labelTask+"="+taskLabel(task.TaskID), task.TaskID)
		if err != nil {
			t.Fatalf("list pods: %v", err)
		}
		return len(pods) == 0, fmt.Sprintf("%d Pod(s) left", len(pods))
	})
	t.Log("the Pod is gone, and Destroy was never called")

	// The failed Job stays for its time to live, then Kubernetes removes it.
	waitFor(t, 3*time.Minute, "Kubernetes removes the Job after its time to live", func() (bool, string) {
		_, err := b.getJob(ctx, task.TaskID)
		if errors.Is(err, core.ErrNotFound) {
			return true, ""
		}
		if err != nil {
			t.Fatalf("get the job: %v", err)
		}
		return false, "the Job still exists"
	})
	t.Log("the Job is gone")

	// The task is now unknown to the backend.
	if _, err := b.Status(ctx, task.TaskID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Status after the removal: %v, want ErrNotFound", err)
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// runtimeArch is the platform value of the task. The backend does not use it
// to place the Pod. Task.Validate only checks that it is amd64 or arm64.
func runtimeArch() string {
	if v := os.Getenv("OF_E2E_ARCH"); v != "" {
		return v
	}
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// waitFor polls fn every second until it is true or the time passes.
func waitFor(t *testing.T, d time.Duration, what string, fn func() (bool, string)) {
	t.Helper()
	deadline := time.Now().Add(d)
	var last string
	for {
		ok, info := fn()
		if ok {
			return
		}
		last = info
		if time.Now().After(deadline) {
			t.Fatalf("timeout after %s: %s (%s)", d, what, last)
		}
		time.Sleep(time.Second)
	}
}

// roleIdentity makes a service account with a Role in the namespace, and
// returns a configuration that uses only the token of that account.
func (e *k8sEnv) roleIdentity(name string, rules []any) *kubernetes.RestConfig {
	e.t.Helper()
	ctx := context.Background()
	for _, o := range []struct {
		kind string
		obj  map[string]any
	}{
		{"serviceaccounts", map[string]any{
			"apiVersion": "v1", "kind": "ServiceAccount",
			"metadata": map[string]any{"name": name, "namespace": e.ns},
		}},
	} {
		if err := e.call(ctx, http.MethodPost, e.path(o.kind), o.obj, nil); err != nil {
			e.t.Fatalf("create %s: %v", o.kind, err)
		}
	}
	rbac := "/apis/rbac.authorization.k8s.io/v1/namespaces/" + url.PathEscape(e.ns)
	if err := e.call(ctx, http.MethodPost, rbac+"/roles", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "Role",
		"metadata": map[string]any{"name": name, "namespace": e.ns},
		"rules":    rules,
	}, nil); err != nil {
		e.t.Fatalf("create the Role: %v", err)
	}
	if err := e.call(ctx, http.MethodPost, rbac+"/rolebindings", map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": name, "namespace": e.ns},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": name, "namespace": e.ns}},
	}, nil); err != nil {
		e.t.Fatalf("create the RoleBinding: %v", err)
	}
	// The TokenRequest API gives a short-lived token, as `kubectl create token` does.
	var tr struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	if err := e.call(ctx, http.MethodPost, e.path("serviceaccounts/"+name+"/token"), map[string]any{
		"apiVersion": "authentication.k8s.io/v1", "kind": "TokenRequest",
		"spec": map[string]any{"expirationSeconds": 900},
	}, &tr); err != nil || tr.Status.Token == "" {
		e.t.Fatalf("request a token for the service account: %v", err)
	}
	id := *e.cfg
	id.Token, id.TokenFile, id.Username, id.Password = tr.Status.Token, "", "", ""
	// The identity must be the account, not the client certificate of the
	// person who runs the test.
	tc := id.TLS.Clone()
	tc.Certificates = nil
	tc.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
		return &tls.Certificate{}, nil
	}
	id.TLS = tc
	return &id
}

// kubeconfigFor writes a kubeconfig file for a token identity of the cluster.
// The cluster CA comes from the kubeconfig of the person who runs the test.
func (e *k8sEnv) kubeconfigFor(id *kubernetes.RestConfig, dir string) string {
	e.t.Helper()
	// The CA is not in RestConfig as PEM, so the file points to the CA with
	// the same kubeconfig through kubectl-less means: ask the API server for
	// nothing and use insecure-skip-tls-verify only when the test cluster has
	// a local address. For other clusters, set OF_E2E_CA_FILE.
	ca := os.Getenv("OF_E2E_CA_FILE")
	cluster := "    server: " + id.Server + "\n"
	switch {
	case ca != "":
		cluster += "    certificate-authority: " + ca + "\n"
	case strings.Contains(id.Server, "://127.0.0.1") || strings.Contains(id.Server, "://localhost") || strings.Contains(id.Server, "://[::1]"):
		cluster += "    insecure-skip-tls-verify: true\n"
	default:
		e.t.Skip("set OF_E2E_CA_FILE (the CA file of the cluster) to run the server binary against this cluster")
	}
	cfg := "apiVersion: v1\nkind: Config\ncurrent-context: id\n" +
		"clusters:\n- name: c\n  cluster:\n" + cluster +
		"users:\n- name: u\n  user:\n    token: " + id.Token + "\n" +
		"contexts:\n- name: id\n  context:\n    cluster: c\n    user: u\n    namespace: " + e.ns + "\n"
	path := filepath.Join(dir, "kubeconfig-"+e.ns)
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// TestK8sRoleWithoutJobs gives the backend the identity of a service account
// whose Role has every rule that the backend needs except the one for Jobs. It
// runs the startup check of the backend (Preflight, which the server runs
// before it serves) and then the server binary itself. Both must stop with an
// error that names the rule: the resource jobs and the verbs.
func TestK8sRoleWithoutJobs(t *testing.T) {
	e := newK8sEnv(t, "nojobs")
	e.waitServiceAccount("default")
	rules := []any{
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods"}, "verbs": []any{"get", "list", "watch", "delete"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"pods/log"}, "verbs": []any{"get"}},
		map[string]any{"apiGroups": []any{""}, "resources": []any{"events"}, "verbs": []any{"get", "list", "watch"}},
		map[string]any{"apiGroups": []any{"networking.k8s.io"}, "resources": []any{"networkpolicies"}, "verbs": []any{"create", "get", "patch", "delete"}},
	}
	id := e.roleIdentity("sandbox-no-jobs", rules)

	t.Run("Preflight", func(t *testing.T) {
		b, err := New(Options{Config: id, Namespace: e.ns, Logger: slog.New(slog.NewTextHandler(os.Stderr, nil))})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		err = b.Preflight(ctx)
		if err == nil {
			t.Fatal("Preflight passed, but the Role has no rule for jobs")
		}
		t.Logf("Preflight error: %v", err)
		for _, want := range []string{"jobs", "batch", "create", "delete", "Role", e.ns} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error does not contain %q", want)
			}
		}
		// The rules that exist must not be named.
		for _, not := range []string{"pods", "events"} {
			if strings.Contains(err.Error(), not) {
				t.Errorf("the error names %q, a rule that the Role has", not)
			}
		}
	})

	t.Run("ServerStartup", func(t *testing.T) {
		bin := os.Getenv("OF_E2E_SERVER_BIN")
		dir := t.TempDir()
		if bin == "" {
			bin = filepath.Join(dir, "openfactory")
			build := exec.Command("go", "build", "-o", bin, "./cmd/openfactory")
			build.Dir = filepath.Join("..", "..", "..")
			if out, err := build.CombinedOutput(); err != nil {
				t.Fatalf("build the server: %v\n%s", err, out)
			}
		}
		kc := e.kubeconfigFor(id, dir)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "serve")
		cmd.Env = append(os.Environ(),
			"OF_DEV=true",
			"OF_LISTEN=127.0.0.1:0",
			"OF_DATA_DIR="+filepath.Join(dir, "data"),
			"OF_DATABASE_URL=sqlite://:memory:",
			"OF_SANDBOX_BACKEND=kubernetes",
			"OF_KUBECONFIG="+kc,
			"OF_K8S_NAMESPACE="+e.ns,
			"OF_RUNNER_IMAGE=ghcr.io/openfactory-org/openfactory:e2e",
			"OF_K8S_NETWORK_POLICY_CHECK=off",
		)
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		err := cmd.Run()
		t.Logf("server output:\n%s", out.String())
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("the server did not stop with an exit status: %v", err)
		}
		if ctx.Err() != nil {
			t.Fatal("the server did not stop at startup (timeout)")
		}
		for _, want := range []string{"jobs", "create", "delete", "Role"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the server error does not contain %q", want)
			}
		}
		t.Logf("server exit status %d", ee.ExitCode())
	})
}

// TestK8sToolImages starts a task Job with a tool image and a task image that
// does not hold the tool. The runner binary comes from the runner image. The
// Pod runs as the default user (1000), so the copy of the tool image must work
// for a user that is not root. The test replaces the runner by a shell command
// that needs the tool on the PATH. The command comes after the PATH shell of
// the backend, so the test checks the real PATH setup.
func TestK8sToolImages(t *testing.T) {
	toolImage := os.Getenv("OF_E2E_TOOL_IMAGE")
	runnerImage := os.Getenv("OF_E2E_RUNNER_IMAGE")
	if toolImage == "" || runnerImage == "" {
		t.Skip("set OF_E2E_TOOL_IMAGE and OF_E2E_RUNNER_IMAGE to test the tool image copy")
	}
	tool := os.Getenv("OF_E2E_TOOL_COMMAND")
	if tool == "" {
		tool = "claude"
	}
	taskImage := os.Getenv("OF_E2E_TASK_IMAGE")
	if taskImage == "" {
		taskImage = "ubuntu:22.04"
	}
	e := newK8sEnv(t, "tools")
	e.waitServiceAccount("default")
	pull := os.Getenv("OF_E2E_PULL_POLICY")
	if pull == "" {
		pull = "IfNotPresent"
	}
	b, err := New(Options{
		Config: e.cfg, Namespace: e.ns, ImagePullPolicy: pull,
		RunnerImage: runnerImage, PollInterval: time.Second,
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-e2e-tools-" + hex.EncodeToString(randBytes(3))
	task := core.Task{
		TaskID: core.TaskID(runID, 1), RunID: runID, Session: 1, TeamID: "team-e2e",
		Image:      taskImage,
		ToolImages: []core.TaskImage{{Name: tool, Image: toolImage}},
		Platform:   core.TaskPlatform{OS: "linux", Arch: runtimeArch()},
		Env: map[string]string{
			core.TaskEnvControlURL: "http://unused.invalid", core.TaskEnvRunID: runID,
			core.TaskEnvSession: "1", core.TaskEnvBootstrapToken: "e2e-token",
		},
		MaxSessionSeconds: 120, FailureRetentionSeconds: 300,
	}
	job := b.jobManifest(task)
	containers := dig(t, job, "spec", "template", "spec", "containers").([]any)
	tc := containers[0].(map[string]any)
	cmd := tc["command"].([]any)
	if len(cmd) != 6 || cmd[3] != "openfactory-task" {
		t.Fatalf("command = %v", cmd)
	}
	// "id -u" shows that the Pod does not run as root.
	tc["command"] = append(cmd[:4:4], "/bin/sh", "-c", "test \"$(id -u)\" != 0 && command -v "+tool+" && "+tool+" --version")
	if err := e.call(context.Background(), http.MethodPost, "/apis/batch/v1/namespaces/"+e.ns+"/jobs", job, nil); err != nil {
		t.Fatalf("create the Job: %v", err)
	}
	var final core.TaskStatus
	waitFor(t, 5*time.Minute, "the task exits", func() (bool, string) {
		st, err := b.Status(context.Background(), task.TaskID)
		if err != nil {
			t.Fatalf("status: %v", err)
		}
		final = st
		return st.Exited(), fmt.Sprintf("state %s", st.State)
	})
	if final.Reason != core.SandboxCompleted {
		t.Fatalf("the task ended with reason %q: %s", final.Reason, final.Message)
	}
	t.Logf("the task ran %s from the tool image %s as a user that is not root", tool, toolImage)
}

// newK8sNetEnv makes the test namespace for the network tests.
func newK8sNetEnv(t *testing.T, targets bool) *netEnv {
	t.Helper()
	kctx := os.Getenv("OF_E2E_KUBE_CONTEXT")
	if kctx == "" {
		t.Skip("set OF_E2E_KUBE_CONTEXT to run the tests on a real cluster")
	}
	proxyImage := os.Getenv("OF_E2E_PROXY_IMAGE")
	if proxyImage == "" {
		t.Skip("set OF_E2E_PROXY_IMAGE (the server image, which holds openfactory and iptables)")
	}
	cfg, err := kubernetes.LoadConfig(os.Getenv("OF_E2E_KUBECONFIG"), kctx)
	if err != nil {
		t.Fatalf("load the kubeconfig (context %q): %v", kctx, err)
	}
	return newNetEnv(t, netEnvConfig{
		Cfg: cfg, NamespacePrefix: "of-net-", ProxyImage: proxyImage,
		PullPolicy: os.Getenv("OF_E2E_PULL_POLICY"), Arch: runtimeArch(), Targets: targets,
	})
}

// TestK8sNetEnforcementProbe measures whether the cluster enforces
// NetworkPolicy, with Pods of its own. Then it checks that the probe of the
// backend gives the same verdict, and that OF_K8S_NETWORK_POLICY_ENFORCEMENT
// does what it says: "require" refuses a restricted task on a cluster that does
// not enforce, and "warn" starts it with a warning.
func TestK8sNetEnforcementProbe(t *testing.T) {
	e := newK8sNetEnv(t, true)
	want := e.measureEnforcement()
	checkEnforcementProbe(t, e, want)
	checkEnforcementSetting(t, e, want)
}

// TestK8sNetEgressProxy checks the proxy sidecar. A task reaches the
// allowed host through the proxy and gets 403 for another host. The tests need
// no policy enforcement: the proxy answers on any cluster.
func TestK8sNetEgressProxy(t *testing.T) {
	e := newK8sNetEnv(t, true)
	t.Run("sidecar", func(t *testing.T) {
		r := e.runSidecar("sidecar", Options{})
		r.wantProxy(t)
		r.wantReport(t, e)
	})
}

// TestK8sNetEgressLock checks the iptables lock. The lock lives in the network
// space of the Pod, so it works on any cluster, with or without an enforcing
// network plugin. Without the lock, a program of the task that ignores the
// proxy variables reaches a host on port 80 directly. With the lock, the
// connection is refused, and the proxy path still works.
func TestK8sNetEgressLock(t *testing.T) {
	e := newK8sNetEnv(t, true)
	t.Run("no_lock", func(t *testing.T) {
		r := e.runSidecar("nolock", Options{})
		r.wantProxy(t)
		// The known limit of the sidecar: the policy allows port 80.
		r.want(t, "direct_80", "200", 0)
	})
	t.Run("iptables", func(t *testing.T) {
		r := e.runSidecar("lock", Options{EgressLock: egress.LockIPTables})
		r.wantProxy(t)
		// The lock refuses a direct connection to port 80 at once.
		r.want(t, "direct_80", "", 7, 56)
	})
}

// blockedRC are the curl exit codes of a connection that a NetworkPolicy
// blocks. A plugin that drops the packets (Calico) gives 28, a timeout. A
// plugin that rejects them (the policy controller of k3s) gives 7 at once.
var blockedRC = []int{7, 28}

// TestK8sNetPolicyBlocksDirectEgress checks that the NetworkPolicy of a
// restricted task blocks a direct connection. A cluster that does not enforce
// NetworkPolicy cannot pass this test, so the test skips there.
func TestK8sNetPolicyBlocksDirectEgress(t *testing.T) {
	e := newK8sNetEnv(t, true)
	if got := e.measureEnforcement(); got != egress.Enforced {
		t.Skipf("this cluster does not enforce NetworkPolicy (measured: %s), so a NetworkPolicy cannot block direct egress here: use a cluster with an enforcing network plugin", got)
	}
	t.Run("sidecar", func(t *testing.T) {
		r := e.runSidecar("sidecar", Options{})
		r.wantProxy(t)
		// The policy allows only DNS, the control plane and TCP 80 and 443.
		r.want(t, "direct_8080", "", blockedRC...)
		// The first command of the task, with no delay, is blocked too.
		r.want(t, "first_direct", "", blockedRC...)
	})
	t.Run("sidecar_lock", func(t *testing.T) {
		r := e.runSidecar("lock", Options{EgressLock: egress.LockIPTables})
		r.wantProxy(t)
		r.want(t, "direct_80", "", 7, 56)
		r.want(t, "first_direct", "", blockedRC...)
	})
	// The control case: a task without a restriction has no policy, so the
	// connections work. The blocks above come from the policy.
	t.Run("unrestricted", func(t *testing.T) {
		r := e.runOpen("open")
		r.want(t, "first_direct", "", 0, 52, 56)
		r.want(t, "direct_80", "200", 0)
		r.want(t, "direct_8080", "", 0, 52, 56)
		r.want(t, "control", "200", 0)
	})
}

// TestK8sNetPolicyWaitTimeout checks what happens when the NetworkPolicy does
// not take effect in time: the task fails with network_policy_timeout and its
// command never runs. The test makes this case with a target for the wait that
// the policy allows. It needs no enforcing network plugin.
func TestK8sNetPolicyWaitTimeout(t *testing.T) {
	e := newK8sNetEnv(t, true)
	checkWaitTimeout(t, e)
}
