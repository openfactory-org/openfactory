//go:build e2e_k8s

package k8stask

// Shared code of the network-limit tests of k8s_e2e_test.go. They run on any
// cluster that you already have (OF_E2E_KUBE_CONTEXT), for example the kind
// cluster of the local stack (make local-up with KUBE_CONTEXT=kind-of-local,
// which has Calico). The cluster may or may not enforce NetworkPolicy. The
// tests find out which, and check the right thing.
//
// Variables:
//
//	OF_E2E_PROXY_IMAGE    image that holds the openfactory binary and iptables
//	                      (the server image). It is the proxy sidecar, the
//	                      egress lock and the image of the probe Pods.
//	OF_E2E_CLIENT_IMAGE   image of the task container. It needs sh and curl.
//	                      Default: curlimages/curl:8.10.1. The runner image
//	                      also works.
//	OF_E2E_NGINX_IMAGE    web server for the targets. Default: nginx:1.27-alpine
//	OF_E2E_WEB_COMMAND    a shell command that serves HTTP on port 80. Set it
//	                      when the web image is not a web server. Example for
//	                      an image that holds python3: python3 -m http.server 80
//	OF_E2E_NETPOL_ENFORCED  true or false: what you know about the cluster. The
//	                      tests fail if their own measurement differs.
//	OF_E2E_KEEP           set to 1 to keep the test namespace
//	OF_E2E_NO_WAIT        set to 1 to turn the egress wait off (a negative
//	                      control: the first command of a task can then pass
//	                      the NetworkPolicy that is not active yet)
//
// The task container does not run the runner here. A ConfigMap gives it a
// script that is named openfactory and comes first in PATH, so that the command
// of the Job (`openfactory runner`) runs the script. The script calls curl and
// prints the results. Everything else is the real backend: the Job, the
// NetworkPolicy, the proxy sidecar, the egress lock and the proxy variables.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
)

const (
	netDefaultClientImage = "curlimages/curl:8.10.1"
	netDefaultWebImage    = "nginx:1.27-alpine"
	netShimConfigMap      = "of-e2e-shim"
	netTargetPort         = 8080
)

// The script uses the port 8080 of netTargetPort as a literal.

// netShim is the script that runs as `openfactory runner` in the task
// container. Each line "RESULT <name> rc=<curl exit code> code=<HTTP status>"
// is one check. The checks are:
//
//	first_direct   the very first command of the task: a direct connection to
//	               the Pod on port 8080, with no proxy. There is no wait before
//	               it. A NetworkPolicy that the network plugin applies late must
//	               not let it through: the init container egress-wait holds the
//	               task back until the policy is active.
//	proxy_allowed  GET of an allowed host. The proxy variables apply.
//	proxy_denied   GET of a host that the policy does not allow.
//	direct_80      the same host, with no proxy: a program that ignores the
//	               proxy variables, on port 80.
//	direct_8080    a Pod that listens on another port, with no proxy.
//	control        the control URL, as the runner uses it.
//
// curl exit codes: 0 worked, 7 refused, 28 timed out, 52 or 56 connected and
// then closed (the target of direct_8080 closes at once).
const netShim = `#!/bin/sh
# A network plugin applies a NetworkPolicy a short time after the Pod starts
# (for k3s: some hundred milliseconds). The script does not wait for that: the
# first command is a direct connection. The backend must hold the task back
# until the policy is active.
check() {
  name=$1; shift
  code=$(curl -sS -o /dev/null -w '%{http_code}' --connect-timeout 5 --max-time 15 "$@" 2>/dev/null)
  echo "RESULT $name rc=$? code=$code"
}
check first_direct --noproxy '*' "http://$E2E_TARGET:8080/"
check proxy_allowed "http://$E2E_ALLOWED/"
check proxy_denied "http://$E2E_DENIED/"
check direct_80 --noproxy '*' "http://$E2E_DENIED/"
check direct_8080 --noproxy '*' "http://$E2E_TARGET:8080/"
check control "$OF_CONTROL_URL/"
if [ -n "${OF_NETGUARD_REPORT:-}" ] && curl -s --noproxy '*' --max-time 5 "$OF_NETGUARD_REPORT" >/tmp/report 2>/dev/null; then
  echo "REPORT $(cat /tmp/report)"
fi
exit 0
`

// netEnvConfig is what a suite gives to newNetEnv.
type netEnvConfig struct {
	Cfg *kubernetes.RestConfig
	// NamespacePrefix starts the name of the test namespace.
	NamespacePrefix string
	// ProxyImage holds the openfactory binary and iptables.
	ProxyImage string
	// PullPolicy is the image pull policy of every Pod.
	PullPolicy string
	// Arch is the platform arch of the tasks (amd64 or arm64).
	Arch string
	// Targets starts the target Pods.
	Targets bool
}

// netEnv is one test namespace with its target Pods.
type netEnv struct {
	t           *testing.T
	cfg         *kubernetes.RestConfig
	ns          string
	pull        string
	arch        string
	proxyImage  string
	clientImage string
	webImage    string
	webCommand  string
	// api is a backend that only gives access to the API of the namespace.
	api *Backend

	allowedIP, deniedIP, controlIP, targetIP string

	measured egress.Enforcement
}

func netEnvOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// newNetEnv makes a namespace and, with Targets, starts these Pods: two web
// servers (allowed and denied), a third one that stands for the control plane,
// and a Pod that listens on port 8080.
func newNetEnv(t *testing.T, c netEnvConfig) *netEnv {
	t.Helper()
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	e := &netEnv{
		t: t, cfg: c.Cfg, ns: c.NamespacePrefix + hex.EncodeToString(rnd[:]), proxyImage: c.ProxyImage,
		pull: c.PullPolicy, arch: c.Arch,
		clientImage: netEnvOr("OF_E2E_CLIENT_IMAGE", netDefaultClientImage),
		webImage:    netEnvOr("OF_E2E_NGINX_IMAGE", netDefaultWebImage),
		webCommand:  os.Getenv("OF_E2E_WEB_COMMAND"),
	}
	if e.pull == "" {
		e.pull = "IfNotPresent"
	}
	if e.arch == "" {
		e.arch = "amd64"
	}
	var err error
	e.api, err = New(Options{Config: e.cfg, Namespace: e.ns, ImagePullPolicy: e.pull})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.call(ctx, http.MethodPost, "/api/v1/namespaces", map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": e.ns},
	}, nil); err != nil {
		t.Fatalf("create the namespace %s: %v", e.ns, err)
	}
	t.Logf("namespace %s", e.ns)
	t.Cleanup(func() {
		if os.Getenv("OF_E2E_KEEP") == "1" {
			t.Logf("namespace %s stays (OF_E2E_KEEP=1)", e.ns)
			return
		}
		cctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := e.call(cctx, http.MethodDelete, "/api/v1/namespaces/"+e.ns, nil, nil); err != nil && !errors.Is(err, core.ErrNotFound) {
			t.Logf("delete the namespace %s: %v", e.ns, err)
		}
	})
	e.waitDefaultServiceAccount()
	if err := e.call(ctx, http.MethodPost, e.path("configmaps"), map[string]any{
		"apiVersion": "v1", "kind": "ConfigMap",
		"metadata": map[string]any{"name": netShimConfigMap, "namespace": e.ns},
		"data":     map[string]any{"openfactory": netShim},
	}, nil); err != nil {
		t.Fatalf("create the script: %v", err)
	}
	if c.Targets {
		e.allowedIP = e.startPod("allowed", nil, e.web())
		e.deniedIP = e.startPod("denied", nil, e.web())
		e.controlIP = e.startPod("control", nil, e.web())
		e.targetIP = e.startPod("target", nil, map[string]any{
			"name": "target", "image": e.proxyImage, "imagePullPolicy": e.pull,
			"command":        []any{"openfactory", "netprobe", "serve", "--listen", ":" + strconv.Itoa(netTargetPort)},
			"readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": netTargetPort}, "periodSeconds": 1},
		})
	}
	return e
}

func (e *netEnv) path(kind string) string {
	return "/api/v1/namespaces/" + url.PathEscape(e.ns) + "/" + kind
}

func (e *netEnv) netpolPath() string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(e.ns) + "/networkpolicies"
}

func (e *netEnv) call(ctx context.Context, method, path string, body, out any) error {
	return e.api.do(ctx, method, path, "", body, out)
}

func (e *netEnv) waitDefaultServiceAccount() {
	e.t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		err := e.call(context.Background(), http.MethodGet, e.path("serviceaccounts/default"), nil, nil)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the namespace has no default service account: %v", err)
		}
		time.Sleep(time.Second)
	}
}

// web is the container of a web server on port 80.
func (e *netEnv) web() map[string]any {
	c := map[string]any{
		"name": "web", "image": e.webImage, "imagePullPolicy": e.pull,
		"readinessProbe": map[string]any{"tcpSocket": map[string]any{"port": 80}, "periodSeconds": 1},
	}
	if e.webCommand != "" {
		c["command"] = []any{"sh", "-c", e.webCommand}
	}
	return c
}

// podObject is a Pod with one container that never restarts.
func (e *netEnv) podObject(name string, labels map[string]any, container map[string]any) map[string]any {
	meta := map[string]any{"name": name, "namespace": e.ns}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{
		"apiVersion": "v1", "kind": "Pod", "metadata": meta,
		"spec": map[string]any{
			"restartPolicy":                "Never",
			"automountServiceAccountToken": false,
			"containers":                   []any{container},
		},
	}
}

type netPodStatus struct {
	Status struct {
		Phase      string `json:"phase"`
		PodIP      string `json:"podIP"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

// startPod creates a Pod and waits until it is ready. It returns the Pod IP.
func (e *netEnv) startPod(name string, labels map[string]any, container map[string]any) string {
	e.t.Helper()
	ctx := context.Background()
	if err := e.call(ctx, http.MethodPost, e.path("pods"), e.podObject(name, labels, container), nil); err != nil {
		e.t.Fatalf("create the Pod %s: %v", name, err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var p netPodStatus
		if err := e.call(ctx, http.MethodGet, e.path("pods/"+name), nil, &p); err != nil {
			e.t.Fatalf("read the Pod %s: %v", name, err)
		}
		for _, c := range p.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" && p.Status.PodIP != "" {
				return p.Status.PodIP
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the Pod %s is not ready (phase %q): check that the cluster can pull its image", name, p.Status.Phase)
		}
		time.Sleep(time.Second)
	}
}

// template is the Pod template of the backend: the script, first in PATH.
func (e *netEnv) template() string {
	tmpl := map[string]any{
		"volumes": []any{map[string]any{
			"name": "shim", "configMap": map[string]any{"name": netShimConfigMap, "defaultMode": 0o755},
		}},
		"containers": []any{map[string]any{
			"name": taskContainer,
			"env": []any{map[string]any{
				"name": "PATH", "value": "/shim:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
			}},
			"volumeMounts": []any{map[string]any{"name": "shim", "mountPath": "/shim"}},
		}},
	}
	b, err := json.Marshal(tmpl)
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *netEnv) backend(opts Options) *Backend {
	e.t.Helper()
	opts.Config, opts.Namespace, opts.ImagePullPolicy = e.cfg, e.ns, e.pull
	opts.PodTemplate = e.template()
	// The wait runs `openfactory netprobe` from an image that holds it. The task
	// image of these tests (curl) does not.
	if opts.EgressWaitImage == "" {
		opts.EgressWaitImage = e.proxyImage
	}
	// A plugin that always allows Pod-to-node traffic (the Amazon VPC CNI)
	// never blocks the default target, the kubelet of the node.
	if opts.EgressWaitAddr == "" {
		opts.EgressWaitAddr = os.Getenv("OF_E2E_EGRESS_WAIT_ADDR")
	}
	switch {
	case os.Getenv("OF_E2E_NO_WAIT") == "1":
		opts.EgressWaitTimeout = -1
	case opts.EgressWaitTimeout == 0 && e.allowedIP != "" && e.measureEnforcement() != egress.Enforced:
		// A cluster that does not enforce NetworkPolicy never blocks the target
		// of the wait. The backend skips the wait there when its own check has
		// found that. These tests do not run that check.
		opts.EgressWaitTimeout = -1
	}
	opts.PollInterval = time.Second
	opts.StopGrace = 30 * time.Second
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	b, err := New(opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return b
}

// task returns a task with a restricted policy that allows the allowed host.
// With restricted false, the task is unrestricted.
func (e *netEnv) task(name string, restricted bool) core.Task {
	var rnd [3]byte
	_, _ = rand.Read(rnd[:])
	runID := "run-e2e-" + name + "-" + hex.EncodeToString(rnd[:])
	t := core.Task{
		TaskID: core.TaskID(runID, 1), RunID: runID, Session: 1, TeamID: "team-e2e",
		Image:    e.clientImage,
		Platform: core.TaskPlatform{OS: "linux", Arch: e.arch},
		Env: map[string]string{
			core.TaskEnvControlURL:     "http://" + e.controlIP + ":80",
			core.TaskEnvRunID:          runID,
			core.TaskEnvSession:        "1",
			core.TaskEnvBootstrapToken: "e2e-token",
			"E2E_ALLOWED":              e.allowedIP,
			"E2E_DENIED":               e.deniedIP,
			"E2E_TARGET":               e.targetIP,
		},
		MaxSessionSeconds: 300, FailureRetentionSeconds: 600,
		NetworkPolicy: &core.TaskNetworkPolicy{Mode: egress.ModeUnrestricted},
	}
	if restricted {
		t.NetworkPolicy = &core.TaskNetworkPolicy{Mode: "restricted", AllowedHosts: []string{e.allowedIP}}
	}
	return t
}

type netResult struct {
	rc   int
	code string
}

// netRun is the outcome of one task.
type netRun struct {
	results map[string]netResult
	report  string
	logs    string
}

// want checks one result. An empty code is not checked. rcs lists the curl exit
// codes that are right.
func (r netRun) want(t *testing.T, name, code string, rcs ...int) {
	t.Helper()
	got, ok := r.results[name]
	if !ok {
		t.Errorf("%s: no result in the task log:\n%s", name, r.logs)
		return
	}
	okRC := false
	for _, rc := range rcs {
		okRC = okRC || got.rc == rc
	}
	if !okRC || (code != "" && got.code != code) {
		t.Errorf("%s: curl exit code %d and HTTP status %s, want exit code in %v and status %q", name, got.rc, got.code, rcs, code)
	}
}

func parseNetLog(logs string) netRun {
	r := netRun{results: map[string]netResult{}, logs: logs}
	for _, line := range strings.Split(logs, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 4 && f[0] == "RESULT":
			rc, err := strconv.Atoi(strings.TrimPrefix(f[2], "rc="))
			if err == nil {
				r.results[f[1]] = netResult{rc: rc, code: strings.TrimPrefix(f[3], "code=")}
			}
		case len(f) >= 1 && f[0] == "REPORT":
			r.report = strings.TrimSpace(strings.TrimPrefix(line, "REPORT"))
		}
	}
	return r
}

// netGet calls the API and returns the raw answer, for example a Pod log.
func netGet(ctx context.Context, b *Backend, path string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.cfg.Server+path, nil)
	if err != nil {
		return "", err
	}
	if err := b.cfg.Authorize(req.Header); err != nil {
		return "", err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return string(data), nil
}

func (e *netEnv) podLog(b *Backend, pod, container string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	q := url.Values{"container": {container}}
	out, err := netGet(ctx, b, e.path("pods/"+url.PathEscape(pod)+"/log")+"?"+q.Encode())
	if err != nil {
		return "(no log: " + err.Error() + ")"
	}
	return out
}

// dump writes the logs of the containers of a task to the test log.
func (e *netEnv) dump(b *Backend, st core.TaskStatus) {
	if st.SandboxID == "" {
		return
	}
	for _, c := range []string{taskContainer, egress.ProxySidecarName, egress.LockName, egress.WaitName} {
		e.t.Logf("log of the container %s:\n%s", c, e.podLog(b, st.SandboxID, c))
	}
}

func (e *netEnv) waitExit(b *Backend, taskID string) core.TaskStatus {
	e.t.Helper()
	deadline := time.Now().Add(8 * time.Minute)
	for {
		st, err := b.Status(context.Background(), taskID)
		if err != nil {
			e.t.Fatalf("status of %s: %v", taskID, err)
		}
		if st.Exited() {
			return st
		}
		if time.Now().After(deadline) {
			e.dump(b, st)
			e.t.Fatalf("task %s did not end in time (state %s, reason %q)", taskID, st.State, st.Reason)
		}
		time.Sleep(2 * time.Second)
	}
}

func (e *netEnv) netpolOf(b *Backend, task core.Task) (owner string, err error) {
	var np struct {
		Metadata struct {
			OwnerReferences []struct {
				Kind string `json:"kind"`
			} `json:"ownerReferences"`
		} `json:"metadata"`
	}
	err = e.call(context.Background(), http.MethodGet, b.netpolsPath()+"/"+url.PathEscape(jobName(task.TaskID)), nil, &np)
	if err == nil && len(np.Metadata.OwnerReferences) > 0 {
		owner = np.Metadata.OwnerReferences[0].Kind
	}
	return owner, err
}

// run starts the task and waits until it ends. It checks that a restricted
// task has a NetworkPolicy that its Job owns, and that Destroy removes the
// Job and the NetworkPolicy.
func (e *netEnv) run(b *Backend, task core.Task) netRun {
	e.t.Helper()
	ctx := context.Background()
	if _, err := b.Start(ctx, task); err != nil {
		e.t.Fatalf("start %s: %v", task.TaskID, err)
	}
	e.t.Cleanup(func() { _ = b.Destroy(context.Background(), task.TaskID) })

	if restricted(task) {
		owner, err := e.netpolOf(b, task)
		switch {
		case err != nil:
			e.t.Errorf("the restricted task has no NetworkPolicy: %v", err)
		case owner != "Job":
			e.t.Errorf("the NetworkPolicy has owner %q, want the Job", owner)
		}
	} else if _, err := e.netpolOf(b, task); !errors.Is(err, core.ErrNotFound) {
		e.t.Errorf("an unrestricted task has a NetworkPolicy (error %v)", err)
	}

	st := e.waitExit(b, task.TaskID)
	logs := e.podLog(b, st.SandboxID, taskContainer)
	if st.Reason != core.SandboxCompleted {
		e.dump(b, st)
		e.t.Fatalf("task %s ended with reason %q (exit code %v): %s", task.TaskID, st.Reason, st.ExitCode, st.Message)
	}
	e.t.Logf("log of the task container:\n%s", logs)
	res := parseNetLog(logs)

	if err := b.Destroy(ctx, task.TaskID); err != nil {
		e.t.Errorf("destroy %s: %v", task.TaskID, err)
	}
	if _, err := e.netpolOf(b, task); !errors.Is(err, core.ErrNotFound) {
		e.t.Errorf("the NetworkPolicy is still there after Destroy (error %v)", err)
	}
	if _, err := b.Status(ctx, task.TaskID); !errors.Is(err, core.ErrNotFound) {
		e.t.Errorf("the Job is still there after Destroy (error %v)", err)
	}
	return res
}

// runClientPod runs a script in a Pod of the client image and returns its log.
// It waits until the Pod ends. The labels select the Pod for a policy.
func (e *netEnv) runClientPod(name string, labels map[string]any, script string) string {
	e.t.Helper()
	ctx := context.Background()
	pod := e.podObject(name, labels, map[string]any{
		"name": "client", "image": e.clientImage, "imagePullPolicy": e.pull,
		"command": []any{"sh", "-c", script},
	})
	if err := e.call(ctx, http.MethodPost, e.path("pods"), pod, nil); err != nil {
		e.t.Fatalf("create the Pod %s: %v", name, err)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for {
		var p netPodStatus
		if err := e.call(ctx, http.MethodGet, e.path("pods/"+name), nil, &p); err != nil {
			e.t.Fatalf("read the Pod %s: %v", name, err)
		}
		if p.Status.Phase == "Succeeded" || p.Status.Phase == "Failed" {
			break
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("the Pod %s did not end (phase %q): check that the cluster can pull its image", name, p.Status.Phase)
		}
		time.Sleep(time.Second)
	}
	return e.podLog(e.api, name, "client")
}

// measureEnforcement finds out, with two client Pods that the test starts
// itself, whether the cluster enforces NetworkPolicy. The probe of the
// backend is not used: the test checks that probe against this measurement.
// A policy that denies all egress applies to one client. Both clients connect
// to a web server. The client with no policy must connect. The other one must
// fail if the cluster enforces policies. The result is cached.
func (e *netEnv) measureEnforcement() egress.Enforcement {
	e.t.Helper()
	if e.measured != "" {
		return e.measured
	}
	if e.allowedIP == "" {
		e.t.Fatal("measureEnforcement needs the target Pods")
	}
	const label = "of-e2e-measure"
	if err := e.call(context.Background(), http.MethodPost, e.netpolPath(), map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": "of-e2e-measure-deny", "namespace": e.ns},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{label: "denied"}},
			"policyTypes": []any{"Egress"},
		},
	}, nil); err != nil {
		e.t.Fatalf("create the deny policy: %v", err)
	}
	// The wait gives the network plugin time to apply the policy.
	script := "sleep 8; for i in 1 2 3; do curl -sS -o /dev/null --noproxy '*' --connect-timeout 4 http://" +
		e.allowedIP + "/ >/dev/null 2>&1; rc=$?; [ $rc -eq 0 ] && break; done; echo \"MEASURE rc=$rc\""
	rc := func(log string) (int, bool) {
		for _, line := range strings.Split(log, "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "MEASURE rc="); ok {
				n, err := strconv.Atoi(v)
				return n, err == nil
			}
		}
		return 0, false
	}
	controlLog := e.runClientPod("measure-control", nil, script)
	deniedLog := e.runClientPod("measure-denied", map[string]any{label: "denied"}, script)
	cRC, cOK := rc(controlLog)
	dRC, dOK := rc(deniedLog)
	if !cOK || !dOK || cRC != 0 {
		e.t.Fatalf("cannot measure NetworkPolicy enforcement: the control Pod must reach the web server.\ncontrol log: %s\ndenied log: %s", controlLog, deniedLog)
	}
	e.measured = egress.NotEnforced
	if dRC != 0 {
		e.measured = egress.Enforced
	}
	e.t.Logf("measured by the test: a Pod with a deny-all egress policy gets curl exit code %d, a Pod without a policy gets 0: NetworkPolicy is %s", dRC, e.measured)
	if v := os.Getenv("OF_E2E_NETPOL_ENFORCED"); v != "" {
		want := egress.NotEnforced
		if v == "true" || v == "1" {
			want = egress.Enforced
		}
		if want != e.measured {
			e.t.Fatalf("OF_E2E_NETPOL_ENFORCED=%s, but the measurement says %s", v, e.measured)
		}
	}
	return e.measured
}

// tryStart starts a task and does not wait for it. A task that started is
// destroyed at the end of the test.
func (e *netEnv) tryStart(b *Backend, task core.Task) error {
	e.t.Helper()
	_, err := b.Start(context.Background(), task)
	if err == nil {
		e.t.Cleanup(func() { _ = b.Destroy(context.Background(), task.TaskID) })
	}
	return err
}

// ---- scenarios and checks that both suites use ----

// checkEnforcementProbe runs the probe against the cluster and checks that it
// finds want. It also runs the checks that the backend does at startup (the
// Role, the probe and the test Job) and the gate that refuses a restricted task
// on a cluster that is not known to enforce policies.
func checkEnforcementProbe(t *testing.T, e *netEnv, want egress.Enforcement) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	got, err := egress.Probe(ctx, egress.ProbeParams{
		Do: func(ctx context.Context, method, path string, body, out any) error {
			return e.call(ctx, method, path, body, out)
		},
		Namespace: e.ns, Image: e.proxyImage, PullPolicy: e.pull, Logger: slog.Default(),
	})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got != want {
		t.Fatalf("probe = %q, want %q", got, want)
	}
	// The probe removes its Pods and its policy.
	for _, kind := range []string{"pods", "networkpolicies"} {
		path := e.path(kind)
		if kind == "networkpolicies" {
			path = e.netpolPath()
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		q := url.Values{"labelSelector": {egress.LabelProbe}}
		if err := e.call(ctx, http.MethodGet, path+"?"+q.Encode(), nil, &list); err != nil {
			t.Fatalf("list the probe %s: %v", kind, err)
		}
		if len(list.Items) != 0 {
			t.Errorf("the probe left %d %s behind", len(list.Items), kind)
		}
	}

	b := e.backend(Options{
		EgressProxySidecarImage: e.proxyImage,
		NetworkPolicyCheck:      egress.CheckProbe, NetworkPolicyEnforcement: egress.EnforcementRequire,
		PreflightJob: true, PreflightImage: e.clientImage, PreflightTimeout: 5 * time.Minute,
	})
	if err := b.Preflight(ctx); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if known, ok := b.NetworkPolicyAccess(); !known || !ok {
		t.Errorf("NetworkPolicyAccess = known %v, ok %v, want both true", known, ok)
	}
	st, err := b.enforce.Result(ctx)
	if err != nil || st != want {
		t.Fatalf("the check of the backend = %q, %v, want %q", st, err, want)
	}
	if got := b.NetworkPolicyEnforcement(); got != want {
		t.Errorf("NetworkPolicyEnforcement = %q, want %q", got, want)
	}
	gateErr := egress.Gate(ctx, b.enforce, egress.EnforcementRequire, false, slog.Default(), "e2e")
	switch {
	case want == egress.Enforced && gateErr != nil:
		t.Errorf("the gate refuses a restricted task on an enforcing cluster: %v", gateErr)
	case want != egress.Enforced && !errors.Is(gateErr, core.ErrInvalid):
		t.Errorf("the gate = %v, want an error that wraps core.ErrInvalid on a cluster that does not enforce", gateErr)
	}
}

// checkEnforcementSetting checks what the platform does with a restricted task
// for each value of OF_K8S_NETWORK_POLICY_ENFORCEMENT, with the probe as the
// check. want is what the cluster does.
//
//	require  The task starts if the cluster enforces policies. Else Start
//	         fails with core.ErrInvalid, and no Job and no NetworkPolicy stay.
//	warn     The task starts. If the cluster does not enforce policies, the
//	         backend logs a warning. If it does, it logs none.
func checkEnforcementSetting(t *testing.T, e *netEnv, want egress.Enforcement) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	t.Run("require", func(t *testing.T) {
		b := e.backend(Options{
			EgressProxySidecarImage: e.proxyImage,
			NetworkPolicyCheck:      egress.CheckProbe, NetworkPolicyEnforcement: egress.EnforcementRequire,
		})
		if err := b.Preflight(ctx); err != nil {
			t.Fatalf("preflight: %v", err)
		}
		task := e.task("require", true)
		err := e.tryStart(b, task)
		if want == egress.Enforced {
			if err != nil {
				t.Fatalf("Start with require on an enforcing cluster: %v", err)
			}
			return
		}
		if !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("Start with require on a cluster that does not enforce = %v, want an error that wraps core.ErrInvalid", err)
		}
		if !strings.Contains(err.Error(), "does not enforce NetworkPolicy") {
			t.Errorf("the error does not say that the cluster does not enforce NetworkPolicy: %v", err)
		}
		t.Logf("Start refused the task: %v", err)
		if _, err := b.Status(ctx, task.TaskID); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("a refused task has a Job (error %v)", err)
		}
		if _, err := e.netpolOf(b, task); !errors.Is(err, core.ErrNotFound) {
			t.Errorf("a refused task has a NetworkPolicy (error %v)", err)
		}
	})

	t.Run("warn", func(t *testing.T) {
		var logs bytes.Buffer
		b := e.backend(Options{
			EgressProxySidecarImage: e.proxyImage,
			NetworkPolicyCheck:      egress.CheckProbe, NetworkPolicyEnforcement: egress.EnforcementWarn,
			Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		})
		if err := b.Preflight(ctx); err != nil {
			t.Fatalf("preflight: %v", err)
		}
		// The warning needs the answer of the check.
		if st, err := b.enforce.Result(ctx); err != nil || st != want {
			t.Fatalf("the check of the backend = %q, %v, want %q", st, err, want)
		}
		if err := e.tryStart(b, e.task("warn", true)); err != nil {
			t.Fatalf("Start with warn: %v", err)
		}
		warned := strings.Contains(logs.String(), "is NOT enforced by the cluster network plugin")
		if want == egress.NotEnforced && !warned {
			t.Errorf("no warning for a restricted task on a cluster that does not enforce. Log:\n%s", logs.String())
		}
		if want == egress.Enforced && warned {
			t.Errorf("a warning on a cluster that enforces. Log:\n%s", logs.String())
		}
	})
}

// runSidecar runs a restricted task with the proxy sidecar. opts may set the
// lock and the check.
func (e *netEnv) runSidecar(name string, opts Options) netRun {
	e.t.Helper()
	opts.EgressProxySidecarImage = e.proxyImage
	return e.run(e.backend(opts), e.task(name, true))
}

// runOpen runs a task without a restriction. It has no NetworkPolicy.
func (e *netEnv) runOpen(name string) netRun {
	e.t.Helper()
	return e.run(e.backend(Options{EgressProxySidecarImage: e.proxyImage}), e.task(name, false))
}

// wantProxy checks the path through the proxy: the allowed host answers, and
// another host gets 403 from the proxy. The control plane answers too.
func (r netRun) wantProxy(t *testing.T) {
	t.Helper()
	r.want(t, "proxy_allowed", "200", 0)
	r.want(t, "proxy_denied", "403", 0)
	r.want(t, "control", "200", 0)
}

// wantReport checks the report of the sidecar: it names the host that the
// proxy refused, and not the allowed host.
func (r netRun) wantReport(t *testing.T, e *netEnv) {
	t.Helper()
	if !strings.Contains(r.report, `"host":"`+e.deniedIP+`"`) {
		t.Errorf("the report of blocked hosts does not name %s: %q", e.deniedIP, r.report)
	}
	if strings.Contains(r.report, `"host":"`+e.allowedIP+`"`) {
		t.Errorf("the report of blocked hosts names the allowed host %s: %q", e.allowedIP, r.report)
	}
}

// checkWaitTimeout checks the failure path of the wait. The target of the wait
// is the control plane. The policy of the task allows it, so the connection
// works for ever and the wait runs out of time. The task must then end with the
// reason network_policy_timeout, which gives the failure code
// network_policy_timeout, and its command must never run.
func checkWaitTimeout(t *testing.T, e *netEnv) {
	t.Helper()
	target := e.controlIP + ":80"
	b := e.backend(Options{EgressWaitAddr: target, EgressWaitTimeout: 4 * time.Second})
	task := e.task("waittimeout", true)
	if err := e.tryStart(b, task); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := e.waitExit(b, task.TaskID)
	if st.Reason != core.SandboxNetworkPolicyTimeout {
		e.dump(b, st)
		t.Fatalf("reason = %q (exit code %v, message %q), want %q", st.Reason, st.ExitCode, st.Message, core.SandboxNetworkPolicyTimeout)
	}
	if st.ExitCode == nil || *st.ExitCode != egress.WaitExitCode {
		t.Errorf("exit code = %v, want %d", st.ExitCode, egress.WaitExitCode)
	}
	if !strings.Contains(st.Message, egress.WaitName) || !strings.Contains(st.Message, target) {
		t.Errorf("the message does not name the wait and its target: %q", st.Message)
	}
	if code, ok := core.FailureForSandboxReason(st.Reason); !ok || code != core.FailureNetworkPolicyTimeout {
		t.Errorf("failure code = %q, %v", code, ok)
	}
	t.Logf("the task ended with %q: %s", st.Reason, st.Message)

	// The task command never ran: the task container never started.
	var pod struct {
		Status struct {
			ContainerStatuses []struct {
				Name  string `json:"name"`
				State struct {
					Waiting    *struct{} `json:"waiting"`
					Running    *struct{} `json:"running"`
					Terminated *struct{} `json:"terminated"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := e.call(context.Background(), http.MethodGet, e.path("pods/"+st.SandboxID), nil, &pod); err != nil {
		t.Fatalf("read the Pod: %v", err)
	}
	for _, c := range pod.Status.ContainerStatuses {
		if c.State.Running != nil || c.State.Terminated != nil {
			t.Errorf("the container %s started although the wait failed", c.Name)
		}
	}
	if log := e.podLog(b, st.SandboxID, taskContainer); strings.Contains(log, "RESULT") {
		t.Errorf("the task command ran: %s", log)
	}
}
