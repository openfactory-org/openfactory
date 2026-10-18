package k8stask

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

func restrictedTask(run string) core.Task {
	t := testTask(run, 1)
	t.NetworkPolicy = &core.TaskNetworkPolicy{Mode: "restricted", AllowedHosts: []string{"example.com"}}
	return t
}

func podSpecOf(t *testing.T, f *fakeAPI, taskID string) map[string]any {
	t.Helper()
	return dig(t, stored(t, f, taskID), "spec", "template", "spec").(map[string]any)
}

// --- NetworkPolicy ---

func TestNetworkPolicyForRestrictedTask(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.ControlPlaneNamespace = "of-system"
		o.ControlPlaneSelector = "app.kubernetes.io/name=openfactory"
	})
	task := restrictedTask("run_np")
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	np, ok := f.netpol(task.TaskID)
	if !ok {
		t.Fatal("no NetworkPolicy")
	}
	if np["apiVersion"] != "networking.k8s.io/v1" || np["kind"] != "NetworkPolicy" {
		t.Errorf("type = %v %v", np["apiVersion"], np["kind"])
	}
	if dig(t, np, "metadata", "name") != jobName(task.TaskID) {
		t.Errorf("name = %v", dig(t, np, "metadata", "name"))
	}
	if !reflect.DeepEqual(dig(t, np, "spec", "podSelector", "matchLabels"), map[string]any{labelTask: "run_np.1"}) {
		t.Errorf("podSelector = %v", dig(t, np, "spec", "podSelector"))
	}
	if !reflect.DeepEqual(dig(t, np, "spec", "policyTypes"), []any{"Egress"}) {
		t.Errorf("policyTypes = %v", dig(t, np, "spec", "policyTypes"))
	}
	egress := dig(t, np, "spec", "egress").([]any)
	if len(egress) != 2 {
		t.Fatalf("egress = %v", egress)
	}
	// DNS: any namespace, UDP and TCP 53.
	if !reflect.DeepEqual(dig(t, egress, 0, "to", 0, "namespaceSelector"), map[string]any{}) {
		t.Errorf("dns peer = %v", egress[0])
	}
	ports := dig(t, egress, 0, "ports").([]any)
	if len(ports) != 2 || dig(t, ports, 0, "protocol") != "UDP" || dig(t, ports, 1, "protocol") != "TCP" ||
		dig(t, ports, 0, "port") != float64(53) || dig(t, ports, 1, "port") != float64(53) {
		t.Errorf("dns ports = %v", ports)
	}
	// Control plane.
	if !reflect.DeepEqual(dig(t, egress, 1, "to", 0, "podSelector", "matchLabels"),
		map[string]any{"app.kubernetes.io/name": "openfactory"}) ||
		!reflect.DeepEqual(dig(t, egress, 1, "to", 0, "namespaceSelector", "matchLabels"),
			map[string]any{"kubernetes.io/metadata.name": "of-system"}) {
		t.Errorf("control plane peer = %v", egress[1])
	}
	// The Job owns the policy.
	refs := dig(t, np, "metadata", "ownerReferences").([]any)
	if len(refs) != 1 || dig(t, refs, 0, "kind") != "Job" || dig(t, refs, 0, "apiVersion") != "batch/v1" ||
		dig(t, refs, 0, "name") != jobName(task.TaskID) || dig(t, refs, 0, "uid") != "uid-"+jobName(task.TaskID) {
		t.Errorf("ownerReferences = %v", refs)
	}
	// The Pod carries the label that the policy selects.
	podLabels := dig(t, stored(t, f, task.TaskID), "spec", "template", "metadata", "labels").(map[string]any)
	if podLabels[labelTask] != "run_np.1" {
		t.Errorf("pod labels = %v", podLabels)
	}

	// Destroy deletes the policy explicitly. The fake has no garbage collector.
	if err := b.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.netpol(task.TaskID); ok || !reflect.DeepEqual(f.netpolDeletes, []string{jobName(task.TaskID)}) {
		t.Errorf("policy after destroy: %v", f.netpols)
	}
	// A second Destroy ignores the 404.
	if err := b.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
}

func TestNetworkPolicyControlURL(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		wantRule  map[string]any // nil: only the DNS rule
	}{
		{"ipv4 https", "https://10.1.2.3", map[string]any{"cidr": "10.1.2.3/32"}},
		{"ipv4 port", "http://10.1.2.3:8080", map[string]any{"cidr": "10.1.2.3/32"}},
		{"ipv6", "http://[fd00::5]:8080", map[string]any{"cidr": "fd00::5/128"}},
		{"host name", "https://factory.internal", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			b := newBackend(t, f, nil)
			task := restrictedTask("run_u")
			task.Env[core.TaskEnvControlURL] = tc.url
			if _, err := b.Start(ctx5(t), task); err != nil {
				t.Fatal(err)
			}
			np, _ := f.netpol(task.TaskID)
			egress := dig(t, np, "spec", "egress").([]any)
			if tc.wantRule == nil {
				if len(egress) != 1 {
					t.Fatalf("egress = %v", egress)
				}
				return
			}
			if len(egress) != 2 || !reflect.DeepEqual(dig(t, egress, 1, "to", 0, "ipBlock"), tc.wantRule) {
				t.Fatalf("egress = %v", egress)
			}
		})
	}
	// The port comes from the URL, or from the scheme.
	if p := dig(t, controlURLRule("http://10.1.2.3:8080"), "ports", 0, "port"); p != 8080 {
		t.Errorf("port = %v", p)
	}
	if p := dig(t, controlURLRule("https://10.1.2.3"), "ports", 0, "port"); p != 443 {
		t.Errorf("port = %v", p)
	}
}

func TestNoNetworkPolicyForUnrestrictedTask(t *testing.T) {
	f := newFakeAPI(t)
	f.forbidNetpol = true // a call would fail
	b := newBackend(t, f, nil)
	for i, policy := range []*core.TaskNetworkPolicy{nil, {Mode: "unrestricted"}} {
		task := testTask("run_free", i+1)
		task.NetworkPolicy = policy
		if _, err := b.Start(ctx5(t), task); err != nil {
			t.Fatalf("policy %v: %v", policy, err)
		}
		if _, ok := f.netpol(task.TaskID); ok {
			t.Errorf("policy %v: NetworkPolicy exists", policy)
		}
	}
	if len(f.netpols) != 0 || f.jobCount() != 2 {
		t.Fatalf("netpols %v jobs %d", f.netpols, f.jobCount())
	}
}

func TestNetworkPolicyCreateForbidden(t *testing.T) {
	f := newFakeAPI(t)
	f.forbidNetpol = true
	b := newBackend(t, f, nil)
	task := restrictedTask("run_deny")
	_, err := b.Start(ctx5(t), task)
	// ErrInvalid makes the dispatcher end the run with invalid_configuration.
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("err = %v, want core.ErrInvalid", err)
	}
	if !strings.Contains(err.Error(), "OF_ALLOW_UNENFORCED_NETWORK_POLICY") || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v", err)
	}
	if f.jobCount() != 0 {
		t.Errorf("a Job was created for a task that cannot be restricted")
	}

	// With the setting, Start works, logs a warning and makes no policy.
	var logs bytes.Buffer
	b2 := newBackend(t, f, func(o *Options) {
		o.AllowUnenforcedNetworkPolicy = true
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	})
	if _, err := b2.Start(ctx5(t), task); err != nil {
		t.Fatalf("start with the setting: %v", err)
	}
	if f.jobCount() != 1 {
		t.Errorf("jobs = %d", f.jobCount())
	}
	if _, ok := f.netpol(task.TaskID); ok {
		t.Error("NetworkPolicy exists")
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "NOT enforced") {
		t.Errorf("no warning in the log: %s", logs.String())
	}
	if strings.Contains(logs.String(), "ofb_secret") {
		t.Error("the bootstrap token is in the log")
	}
	// Destroy works if the cluster never held a policy.
	if err := b2.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkPolicyRemovedWhenJobFails(t *testing.T) {
	f := newFakeAPI(t)
	f.forbidJobPost = true
	b := newBackend(t, f, nil)
	if _, err := b.Start(ctx5(t), restrictedTask("run_nojob")); err == nil {
		t.Fatal("want an error")
	}
	if len(f.netpols) != 0 {
		t.Errorf("a NetworkPolicy stayed: %v", f.netpols)
	}
}

func TestNetworkPolicyStartTwice(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := restrictedTask("run_twice")
	for i := 0; i < 2; i++ {
		if _, err := b.Start(ctx5(t), task); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
	}
	if f.jobCount() != 1 || len(f.netpols) != 1 {
		t.Fatalf("jobs %d netpols %d", f.jobCount(), len(f.netpols))
	}
}

func TestNewPeerErrors(t *testing.T) {
	cfg := startFake(t, newFakeAPI(t))
	for _, o := range []Options{
		{ControlPlaneSelector: "nokey"},
		{ControlPlaneNamespace: "ns"},
		{ActiveDeadline: -time.Second},
	} {
		o.Config = cfg
		if _, err := New(o); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("options %+v: err = %v", o, err)
		}
	}
}

// --- Tool images ---

func TestToolImages(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.RunnerImage = "example.test/runner:1"
		o.PodTemplate = "initContainers:\n- {name: tool-claude, image: old}\n- {name: mine, image: mine}\ncontainers:\n- name: sandbox\n"
	})
	task := testTask("run_tools", 1)
	task.ToolImages = []core.TaskImage{
		{Name: "claude", Image: "example.test/cli-claude:1"},
		{Name: "codex", Image: "example.test/cli-codex:1"},
	}
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	pod := podSpecOf(t, f, task.TaskID)
	inits := pod["initContainers"].([]any)
	var names []string
	for _, c := range inits {
		names = append(names, c.(map[string]any)["name"].(string))
	}
	// The template init container stays. The one that has our name is replaced.
	if !reflect.DeepEqual(names, []string{"mine", "runner-install", "tool-claude", "tool-codex"}) {
		t.Fatalf("init containers = %v", names)
	}
	for i, want := range []struct{ name, image string }{{"claude", "example.test/cli-claude:1"}, {"codex", "example.test/cli-codex:1"}} {
		c := dig(t, inits, 2+i).(map[string]any)
		if c["image"] != want.image ||
			!reflect.DeepEqual(c["command"], []any{"/bin/sh", "-c", "cp -RP /opt/openfactory/tool/. /dest/"}) {
			t.Errorf("init %s = %v", want.name, c)
		}
		if dig(t, c, "volumeMounts", 0, "name") != "tool-"+want.name || dig(t, c, "volumeMounts", 0, "mountPath") != "/dest" {
			t.Errorf("init %s mounts = %v", want.name, c["volumeMounts"])
		}
		if dig(t, c, "securityContext", "runAsNonRoot") != true || dig(t, c, "securityContext", "runAsUser") != float64(1000) ||
			dig(t, c, "securityContext", "allowPrivilegeEscalation") != false {
			t.Errorf("init %s security = %v", want.name, c["securityContext"])
		}
		if c["imagePullPolicy"] != "IfNotPresent" {
			t.Errorf("init %s pull policy = %v", want.name, c["imagePullPolicy"])
		}
	}
	tc := dig(t, pod, "containers", 0).(map[string]any)
	mounts := map[string]string{}
	for _, m := range tc["volumeMounts"].([]any) {
		mounts[m.(map[string]any)["name"].(string)] = m.(map[string]any)["mountPath"].(string)
	}
	if mounts["tool-claude"] != "/opt/openfactory/tools/claude" || mounts["tool-codex"] != "/opt/openfactory/tools/codex" {
		t.Errorf("mounts = %v", mounts)
	}
	vols := map[string]bool{}
	for _, v := range pod["volumes"].([]any) {
		vols[v.(map[string]any)["name"].(string)] = true
	}
	if !vols["tool-claude"] || !vols["tool-codex"] || !vols["runner"] {
		t.Errorf("volumes = %v", vols)
	}
}

// The task container starts a shell that puts the bin directory of each tool
// in front of the PATH of the image, and then starts the runner.
func TestToolImagesPath(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.RunnerImage = "example.test/runner:1" })
	task := testTask("run_path", 1)
	task.ToolImages = []core.TaskImage{
		{Name: "claude", Image: "example.test/cli-claude:1"},
		{Name: "codex", Image: "example.test/cli-codex:1"},
	}
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	pod := podSpecOf(t, f, task.TaskID)
	tc := dig(t, pod, "containers", 0).(map[string]any)
	cmd, _ := tc["command"].([]any)
	if len(cmd) != 6 || cmd[0] != "/bin/sh" || cmd[1] != "-c" || cmd[3] != "openfactory-task" ||
		cmd[4] != "/opt/openfactory/openfactory" || cmd[5] != "runner" {
		t.Fatalf("command = %v", cmd)
	}
	script, _ := cmd[2].(string)
	if !strings.Contains(script, "/opt/openfactory/tools/claude/bin:/opt/openfactory/tools/codex/bin") ||
		!strings.Contains(script, `exec "$@"`) {
		t.Errorf("script = %q", script)
	}
	// The PATH of the image is kept: no PATH variable replaces it.
	for _, e := range tc["env"].([]any) {
		if e.(map[string]any)["name"] == "PATH" {
			t.Errorf("the container sets PATH: %v", e)
		}
	}
	// Without a tool image the command is the runner, as before.
	plain := testTask("run_plain", 1)
	if _, err := b.Start(ctx5(t), plain); err != nil {
		t.Fatal(err)
	}
	pc := dig(t, podSpecOf(t, f, plain.TaskID), "containers", 0, "command")
	if !reflect.DeepEqual(pc, []any{"/opt/openfactory/openfactory", "runner"}) {
		t.Errorf("command without tool images = %v", pc)
	}
}

// The shell script of the task command really changes the PATH.
func TestToolPathScriptRuns(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	script := toolPathScript([]core.TaskImage{{Name: "claude"}, {Name: "codex"}})
	run := func(path string) string {
		cmd := exec.Command(sh, "-c", script, "openfactory-task", "sh", "-c", "printf %s \"$PATH\"")
		cmd.Env = []string{"PATH=" + path}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("run: %v: %s", err, out)
		}
		return string(out)
	}
	if got, want := run("/usr/bin:/bin"), "/opt/openfactory/tools/claude/bin:/opt/openfactory/tools/codex/bin:/usr/bin:/bin"; got != want {
		t.Errorf("PATH = %q, want %q", got, want)
	}
}

// The copy command must not set owner, mode or times of the target directory:
// a user that is not root cannot do that for the root-owned volume. It must
// keep symbolic links, as the CLI images use them.
func TestToolCopyCommandIsPortable(t *testing.T) {
	if strings.Contains(toolCopyCommand, "-a") || strings.Contains(toolCopyCommand, "-p") {
		t.Fatalf("copy command %q preserves attributes", toolCopyCommand)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	root := t.TempDir()
	src, dst := filepath.Join(root, "tool"), filepath.Join(root, "dest")
	for _, d := range []string{filepath.Join(src, "bin"), filepath.Join(src, "lib"), dst} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "lib", "cli.js"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../lib/cli.js", filepath.Join(src, "bin", "cli")); err != nil {
		t.Skip("no symbolic links")
	}
	script := strings.NewReplacer(toolSource, src, toolDest, dst).Replace(toolCopyCommand)
	if out, err := exec.Command(sh, "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v: %s", script, err, out)
	}
	if target, err := os.Readlink(filepath.Join(dst, "bin", "cli")); err != nil || target != "../lib/cli.js" {
		t.Errorf("link = %q, %v", target, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "lib", "cli.js")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("file = %v, %v", fi, err)
	}
}

func TestToolImagesWithoutRunnerImage(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_t2", 1)
	task.ToolImages = []core.TaskImage{{Name: "claude", Image: "example.test/cli:1"}}
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	inits := podSpecOf(t, f, task.TaskID)["initContainers"].([]any)
	if len(inits) != 1 || dig(t, inits, 0, "name") != "tool-claude" {
		t.Fatalf("init containers = %v", inits)
	}
}

func TestToolImagesInvalid(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	for _, images := range [][]core.TaskImage{
		{{Name: "../etc", Image: "x"}},
		{{Name: "Claude", Image: "x"}},
		{{Name: "", Image: "x"}},
		{{Name: strings.Repeat("a", 41), Image: "x"}},
		{{Name: "claude", Image: ""}},
		{{Name: "claude", Image: "x"}, {Name: "claude", Image: "y"}},
	} {
		task := testTask("run_bad", 1)
		task.ToolImages = images
		if _, err := b.Start(ctx5(t), task); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("images %v: err = %v", images, err)
		}
	}
	if f.jobCount() != 0 {
		t.Error("a Job was created")
	}
}

// --- Options in the manifest ---

func TestManifestOptions(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.ExtraLabels = map[string]string{"team": "ops"}
		o.ExtraAnnotations = map[string]string{"cluster-autoscaler.kubernetes.io/safe-to-evict": "false"}
		o.ImagePullPolicy = "Never"
		o.ImagePullSecrets = []string{"regcred", "other"}
		o.RunAsUser = 2000
		o.ActiveDeadline = 30 * time.Minute
		o.WorkspaceSizeLimit = "10Gi"
		o.RunnerImage = "example.test/runner:1"
		o.StopGrace = time.Minute
	})
	task := testTask("run_opt", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	job := stored(t, f, task.TaskID)
	for _, where := range [][]any{{"metadata"}, {"spec", "template", "metadata"}} {
		if dig(t, job, append(where, "labels", "team")...) != "ops" ||
			dig(t, job, append(where, "annotations", "cluster-autoscaler.kubernetes.io/safe-to-evict")...) != "false" {
			t.Errorf("labels or annotations missing at %v", where)
		}
	}
	// The cap is shorter than the session time plus the grace time.
	if dig(t, job, "spec", "activeDeadlineSeconds") != float64(1800) {
		t.Errorf("deadline = %v", dig(t, job, "spec", "activeDeadlineSeconds"))
	}
	pod := podSpecOf(t, f, task.TaskID)
	wantSC := map[string]any{"runAsNonRoot": true, "runAsUser": float64(2000), "runAsGroup": float64(2000), "fsGroup": float64(2000)}
	if !reflect.DeepEqual(pod["securityContext"], wantSC) {
		t.Errorf("securityContext = %v", pod["securityContext"])
	}
	secrets := pod["imagePullSecrets"].([]any)
	if len(secrets) != 2 || dig(t, secrets, 0, "name") != "regcred" || dig(t, secrets, 1, "name") != "other" {
		t.Errorf("imagePullSecrets = %v", secrets)
	}
	if dig(t, pod, "containers", 0, "imagePullPolicy") != "Never" || dig(t, pod, "initContainers", 0, "imagePullPolicy") != "Never" {
		t.Error("pull policy is not Never")
	}
	if dig(t, pod, "initContainers", 0, "securityContext", "runAsUser") != float64(2000) {
		t.Error("init container user")
	}
	for _, v := range pod["volumes"].([]any) {
		m := v.(map[string]any)
		if m["name"] == "workspace" && !reflect.DeepEqual(m["emptyDir"], map[string]any{"sizeLimit": "10Gi"}) {
			t.Errorf("workspace = %v", m)
		}
	}

	// The limit of the task wins. A deadline above the session limit does nothing.
	task2 := testTask("run_opt", 2)
	task2.WorkspaceSizeLimit = "5Gi"
	b.opts.ActiveDeadline = 24 * time.Hour
	m := b.jobManifest(task2)
	if dig(t, m, "spec", "activeDeadlineSeconds") != int64(3600+60) {
		t.Errorf("deadline = %v", dig(t, m, "spec", "activeDeadlineSeconds"))
	}
	for _, v := range dig(t, m, "spec", "template", "spec", "volumes").([]any) {
		if mm := v.(map[string]any); mm["name"] == "workspace" && dig(t, mm, "emptyDir", "sizeLimit") != "5Gi" {
			t.Errorf("workspace = %v", mm)
		}
	}
	// No deadline option: session time plus grace.
	b.opts.ActiveDeadline = 0
	if dig(t, b.jobManifest(task2), "spec", "activeDeadlineSeconds") != int64(3660) {
		t.Error("default deadline")
	}
}

func TestManifestRunAsImageUser(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.RunAsImageUser = true
		o.RunnerImage = "example.test/runner:1"
	})
	task := testTask("run_img", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	pod := podSpecOf(t, f, task.TaskID)
	if pod["securityContext"] != nil {
		t.Errorf("securityContext = %v", pod["securityContext"])
	}
	// The init container still runs as a user that is not root.
	if dig(t, pod, "initContainers", 0, "securityContext", "runAsUser") != float64(1000) ||
		dig(t, pod, "initContainers", 0, "securityContext", "runAsNonRoot") != true {
		t.Errorf("init security = %v", dig(t, pod, "initContainers", 0, "securityContext"))
	}
}

// --- Preflight ---

func TestPreflightJobSucceeds(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.PreflightJob = true
		o.RunnerImage = "example.test/runner:9"
		o.PreflightImage = "example.test/default:1"
		o.ImagePullSecrets = []string{"regcred"}
	})
	var seen map[string]any
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		name := f.waitPreflightJob(t)
		f.mu.Lock()
		seen = f.jobs[name]
		f.jobs[name]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
		f.mu.Unlock()
	}()
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if f.jobCount() != 0 {
		t.Error("the test Job was not deleted")
	}
	if dig(t, seen, "spec", "activeDeadlineSeconds") != float64(120) || dig(t, seen, "spec", "backoffLimit") != float64(0) {
		t.Errorf("job spec = %v", seen["spec"])
	}
	tc := dig(t, seen, "spec", "template", "spec", "containers", 0).(map[string]any)
	if tc["image"] != "example.test/runner:9" ||
		!reflect.DeepEqual(tc["command"], []any{"/bin/sh", "-c", "exit 0"}) {
		t.Errorf("container = %v", tc)
	}
	if dig(t, seen, "spec", "template", "spec", "restartPolicy") != "Never" ||
		dig(t, seen, "spec", "template", "spec", "imagePullSecrets", 0, "name") != "regcred" {
		t.Errorf("pod = %v", dig(t, seen, "spec", "template", "spec"))
	}
	if _, ok := dig(t, seen, "metadata", "labels").(map[string]any)[labelTask]; ok {
		t.Error("the test Job has a task label, so List would show it")
	}
}

// The test Job has the init containers of the tool images, so that it tests
// the copy as a real task does.
func TestPreflightJobToolImages(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) {
		o.PreflightJob = true
		o.RunnerImage = "example.test/runner:9"
		o.PreflightToolImages = []core.TaskImage{{Name: "claude", Image: "example.test/cli-claude:9"}}
	})
	var inits any
	go func() {
		name := f.waitPreflightJob(t)
		f.mu.Lock()
		inits = dig(t, f.jobs[name], "spec", "template", "spec", "initContainers")
		f.jobs[name]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
		f.mu.Unlock()
	}()
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	list, _ := inits.([]any)
	if len(list) != 1 || dig(t, list, 0, "name") != "tool-claude" || dig(t, list, 0, "image") != "example.test/cli-claude:9" ||
		dig(t, list, 0, "command", 2) != toolCopyCommand {
		t.Errorf("init containers = %v", inits)
	}
}

func TestPreflightJobImageDefault(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.PreflightJob = true; o.PreflightImage = "example.test/default:1" })
	var image any
	go func() {
		name := f.waitPreflightJob(t)
		f.mu.Lock()
		image = dig(t, f.jobs[name], "spec", "template", "spec", "containers", 0, "image")
		f.jobs[name]["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Complete", "status": "True"}}}
		f.mu.Unlock()
	}()
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if image != "example.test/default:1" {
		t.Errorf("image = %v", image)
	}
}

func TestPreflightJobImagePullFailure(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.PreflightJob = true; o.RunnerImage = "example.test/missing:1" })
	go func() {
		name := f.waitPreflightJob(t)
		f.addPreflightPod(name, "pf-pod", time.Now().Add(-5*time.Minute), map[string]any{
			"phase": "Pending",
			"containerStatuses": []any{map[string]any{"name": "sandbox", "state": map[string]any{
				"waiting": map[string]any{"reason": "ImagePullBackOff", "message": "pull access denied"}}}},
		})
	}()
	err := b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "image pull") || !strings.Contains(err.Error(), "pull access denied") ||
		!strings.Contains(err.Error(), "example.test/missing:1") {
		t.Fatalf("err = %v", err)
	}
	if f.jobCount() != 0 {
		t.Error("the test Job was not deleted")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.pods) != 0 {
		t.Errorf("pods stayed: %v", f.pods)
	}
}

func TestPreflightJobUnschedulable(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.PreflightJob = true })
	go func() {
		name := f.waitPreflightJob(t)
		f.addPreflightPod(name, "pf-pod", time.Now().Add(-5*time.Minute), map[string]any{
			"phase": "Pending",
			"conditions": []any{map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable",
				"message": "0/3 nodes are available", "lastTransitionTime": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)}},
		})
	}()
	err := b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "unschedulable") || !strings.Contains(err.Error(), "0/3 nodes") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreflightJobProcessFails(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.PreflightJob = true })
	go func() {
		name := f.waitPreflightJob(t)
		f.addPreflightPod(name, "pf-pod", time.Now().Add(-time.Minute), map[string]any{
			"phase": "Failed",
			"containerStatuses": []any{map[string]any{"name": "sandbox", "state": map[string]any{
				"terminated": map[string]any{"exitCode": 127, "reason": "ContainerCannotRun", "message": "no /bin/sh"}}}},
		})
	}()
	err := b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "process_failed") || !strings.Contains(err.Error(), "no /bin/sh") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreflightJobTimeout(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.PreflightJob = true; o.PreflightTimeout = 100 * time.Millisecond })
	err := b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v", err)
	}
	if f.jobCount() != 0 {
		t.Error("the test Job was not deleted")
	}
}

func TestPreflightJobForbidden(t *testing.T) {
	f := newFakeAPI(t)
	f.forbidJobPost = true
	b := newBackend(t, f, func(o *Options) { o.PreflightJob = true })
	err := b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "no permission to create jobs") {
		t.Fatalf("err = %v", err)
	}
}

func TestPreflightJobOffByDefault(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if f.jobCount() != 0 {
		t.Error("Preflight created a Job without OF_K8S_PREFLIGHT_JOB")
	}
}

func TestPreflightNetworkPolicyAccess(t *testing.T) {
	f := newFakeAPI(t)
	var logs bytes.Buffer
	b := newBackend(t, f, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
	if known, _ := b.NetworkPolicyAccess(); known {
		t.Fatal("known before Preflight")
	}
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if known, ok := b.NetworkPolicyAccess(); !known || !ok {
		t.Fatalf("known %v ok %v", known, ok)
	}

	f.reviewDenied["delete"] = true
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if known, ok := b.NetworkPolicyAccess(); !known || ok {
		t.Fatalf("known %v ok %v", known, ok)
	}
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "networkpolicies") {
		t.Errorf("log = %s", logs.String())
	}
	// Start names the finding when the create fails.
	f.forbidNetpol = true
	_, err := b.Start(ctx5(t), restrictedTask("run_hint"))
	if err == nil || !strings.Contains(err.Error(), "preflight found") {
		t.Fatalf("err = %v", err)
	}

	// A failed review leaves the answer unknown. Preflight still passes.
	f2 := newFakeAPI(t)
	f2.reviewFails = true
	b2 := newBackend(t, f2, nil)
	if err := b2.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	if known, _ := b2.NetworkPolicyAccess(); known {
		t.Error("known after a failed review")
	}
}

// --- Status mapping for the life of the run ---

func TestMapStatusLifetime(t *testing.T) {
	opts := Options{ImagePullTimeout: 5 * time.Minute, UnschedulableTimeout: 10 * time.Minute}
	job := func(cond string) string {
		c := ""
		if cond != "" {
			c = fmt.Sprintf(`,"status":{"conditions":[{"type":"Failed","status":"True","reason":%q,"message":"job msg","lastTransitionTime":%q}]}`, cond, ts(-time.Minute))
		}
		return `{"metadata":{"annotations":{"openfactory.dev/sandbox-id":"run_a.1"}}` + c + `}`
	}
	pod := func(extra, status string) string {
		return fmt.Sprintf(`{"metadata":{"name":"pod-1","creationTimestamp":%q%s},"status":%s}`, ts(-time.Hour), extra, status)
	}
	term := func(code int, reason string) string {
		return fmt.Sprintf(`{"phase":"Failed","containerStatuses":[{"name":"sandbox","state":{"terminated":{"exitCode":%d,"reason":%q,"message":"c msg"}}}]}`, code, reason)
	}
	disrupt := func(phase, reason string) string {
		return fmt.Sprintf(`{"phase":%q,"conditions":[{"type":"DisruptionTarget","status":"True","reason":%q,"message":"d msg"}]}`, phase, reason)
	}
	waiting := func(reason string) string {
		return fmt.Sprintf(`{"phase":"Pending","containerStatuses":[{"name":"sandbox","state":{"waiting":{"reason":%q,"message":"w msg"}}}]}`, reason)
	}
	initWaiting := func(reason string) string {
		return fmt.Sprintf(`{"phase":"Pending","initContainerStatuses":[{"name":"tool-claude","state":{"waiting":{"reason":%q,"message":"w msg"}}}]}`, reason)
	}
	deleting := `,"deletionTimestamp":"` + ts(-time.Second) + `"`

	tests := []struct {
		name   string
		job    string
		pod    string
		reason core.SandboxReason
	}{
		{"oom killed", job(""), pod("", term(137, "OOMKilled")), core.SandboxContainerOOM},
		{"oom with job failed", job("BackoffLimitExceeded"), pod("", term(137, "OOMKilled")), core.SandboxContainerOOM},
		{"evicted", job("BackoffLimitExceeded"), pod("", `{"phase":"Failed","reason":"Evicted","message":"node pressure"}`), core.SandboxEvicted},
		{"job deadline", job("DeadlineExceeded"), pod("", `{"phase":"Running"}`), core.SandboxDeadlineExceeded},
		{"pod deadline", job(""), pod("", `{"phase":"Failed","reason":"DeadlineExceeded","message":"active deadline"}`), core.SandboxDeadlineExceeded},
		{"node lost reason", job(""), pod("", `{"phase":"Failed","reason":"NodeLost"}`), core.SandboxNodeLost},
		{"node shutdown reason", job(""), pod("", `{"phase":"Failed","reason":"Terminated","message":"node shutdown"}`), core.SandboxNodeLost},
		{"disruption target taint", job(""), pod("", disrupt("Failed", "DeletionByTaintManager")), core.SandboxNodeLost},
		{"disruption target kubelet", job(""), pod("", disrupt("Failed", "TerminationByKubelet")), core.SandboxNodeLost},
		{"disruption target while deleting", job(""), pod(deleting, disrupt("Running", "DeletionByTaintManager")), core.SandboxNodeLost},
		{"disruption target preemption", job(""), pod("", disrupt("Failed", "PreemptionByScheduler")), core.SandboxEvicted},
		{"disruption target eviction api", job(""), pod("", disrupt("Failed", "EvictionByEvictionAPI")), core.SandboxEvicted},
		{"image pull backoff", job(""), pod("", waiting("ImagePullBackOff")), core.SandboxImagePull},
		{"err image pull", job(""), pod("", waiting("ErrImagePull")), core.SandboxImagePull},
		{"tool image pull", job(""), pod("", initWaiting("ImagePullBackOff")), core.SandboxImagePull},
		{"unschedulable", job(""), pod("", fmt.Sprintf(`{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable","message":"no nodes","lastTransitionTime":%q}]}`, ts(-20*time.Minute))), core.SandboxUnschedulable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mapStatus(decodeJob(t, tc.job), []podObj{decodePod(t, tc.pod)}, mapNow, opts)
			if got.State != core.TaskExited || got.Reason != tc.reason {
				t.Fatalf("got %+v, want reason %s", got, tc.reason)
			}
			if code, ok := core.FailureForSandboxReason(got.Reason); !ok || code == core.FailureAgentProcessFailed {
				t.Errorf("reason %s maps to %q", got.Reason, code)
			}
		})
	}

	t.Run("disruption target on a running Pod is not an end", func(t *testing.T) {
		got := mapStatus(decodeJob(t, job("")), []podObj{decodePod(t, pod("", disrupt("Running", "DeletionByTaintManager")))}, mapNow, opts)
		if got.State == core.TaskExited {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("young image pull error is not an end", func(t *testing.T) {
		young := fmt.Sprintf(`{"metadata":{"name":"pod-1","creationTimestamp":%q},"status":%s}`, ts(-time.Minute), waiting("ErrImagePull"))
		got := mapStatus(decodeJob(t, job("")), []podObj{decodePod(t, young)}, mapNow, opts)
		if got.State == core.TaskExited {
			t.Fatalf("got %+v", got)
		}
	})
}

// TestPreflightNamesMissingRoleRules checks that a Role without a rule of the
// backend stops Preflight, and that the error names the resource and the verb.
func TestPreflightNamesMissingRoleRules(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatalf("a complete Role: %v", err)
	}

	// No rule for jobs at all.
	for _, v := range []string{"create", "get", "list", "watch", "delete"} {
		f.ruleDenied["jobs:"+v] = true
	}
	err := b.Preflight(ctx5(t))
	want := "jobs.batch (create, delete, get, list, watch)"
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "Role") {
		t.Fatalf("err = %v, want it to name %q", err, want)
	}

	// One verb of jobs and one rule of another resource.
	f.ruleDenied = map[string]bool{"jobs:create": true, "pods/log:get": true}
	err = b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "jobs.batch (create)") || !strings.Contains(err.Error(), "pods/log (get)") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "events") {
		t.Errorf("err names a rule that is present: %v", err)
	}

	// If the review itself fails, the list calls decide.
	f.ruleDenied = map[string]bool{"jobs:create": true}
	f.reviewFails = true
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatalf("a failed review must not stop Preflight: %v", err)
	}
}
