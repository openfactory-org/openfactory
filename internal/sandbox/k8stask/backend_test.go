package k8stask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox"
)

func ctx5(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

// dig walks nested maps and slices of a decoded JSON object.
func dig(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("path %v: %q is not in a map (%T)", path, k, v)
			}
			v = m[k]
		case int:
			s, ok := v.([]any)
			if !ok || k >= len(s) {
				t.Fatalf("path %v: index %d is not in a slice (%T)", path, k, v)
			}
			v = s[k]
		}
	}
	return v
}

// stored returns the Job of a task as the API server holds it.
func stored(t *testing.T, f *fakeAPI, taskID string) map[string]any {
	t.Helper()
	j, ok := f.job(taskID)
	if !ok {
		t.Fatalf("no job for %s", taskID)
	}
	return j
}

func envMap(t *testing.T, c any) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range c.(map[string]any)["env"].([]any) {
		m := e.(map[string]any)
		out[m["name"].(string)] = m["value"].(string)
	}
	return out
}

func TestJobName(t *testing.T) {
	short := jobName("run_abc.2")
	if !strings.HasPrefix(short, "of-task-run-abc-2-") {
		t.Fatalf("name = %q", short)
	}
	long1 := jobName(core.TaskID(strings.Repeat("a", 90)+"1", 1))
	long2 := jobName(core.TaskID(strings.Repeat("a", 90)+"2", 1))
	for _, n := range []string{short, long1, long2} {
		if len(n) > 63 || !strings.HasPrefix(n, "of-task-") || n != strings.ToLower(n) ||
			strings.ContainsAny(n, "_.") || strings.HasSuffix(n, "-") {
			t.Errorf("bad name %q (%d)", n, len(n))
		}
	}
	if long1 == long2 {
		t.Fatalf("two long IDs that share a prefix give the same name %q", long1)
	}
	// IDs with the same sanitized form still differ.
	if jobName("a_b.1") == jobName("a.b.1") || jobName("A.1") == jobName("a.1") {
		t.Fatal("IDs with the same sanitized form give the same name")
	}
	// The label stays valid and keeps long IDs apart.
	l1, l2 := taskLabel(strings.Repeat("a", 90)+"1.1"), taskLabel(strings.Repeat("a", 90)+"2.1")
	if len(l1) > 63 || l1 == l2 {
		t.Fatalf("labels %q %q", l1, l2)
	}
	if taskLabel("run_abc.2") != "run_abc.2" {
		t.Fatalf("label = %q", taskLabel("run_abc.2"))
	}
}

func TestStartBuildsJob(t *testing.T) {
	f := newFakeAPI(t)
	tmpl := `
nodeSelector: {disk: ssd}
tolerations:
- {key: gpu, operator: Exists}
imagePullSecrets:
- name: from-template
securityContext: {fsGroup: 2000}
containers:
- name: sandbox
  imagePullPolicy: Always
  env:
  - {name: FROM_TEMPLATE, value: "yes"}
  - {name: EXTRA, value: "template"}
  volumeMounts:
  - {name: cache, mountPath: /cache}
- name: sidecar
  image: example.test/side:1
volumes:
- {name: cache, emptyDir: {}}
`
	b := newBackend(t, f, func(o *Options) {
		o.PodTemplate = tmpl
		o.ImagePullSecrets = []string{"from-template", "from-options"}
		o.ExtraLabels = map[string]string{"team": "ops", labelTask: "wrong"}
		o.ExtraAnnotations = map[string]string{"note": "x"}
		o.RunnerImage = "example.test/runner:1"
		o.StopGrace = 90 * time.Second
	})
	task := testTask("run_abc", 2)
	task.Resources = core.TaskResources{CPU: "2", Memory: "8Gi"}
	task.WorkspaceSizeLimit = "20Gi"
	task.Labels = map[string]string{"custom": "yes", sandbox.LabelManagedBy: "other"}
	task.FailureRetentionSeconds = 600

	st, err := b.Start(ctx5(t), task)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != core.TaskStarting || st.TaskID != task.TaskID {
		t.Fatalf("status = %+v", st)
	}
	job := stored(t, f, task.TaskID)

	if got := dig(t, job, "metadata", "name"); got != jobName(task.TaskID) {
		t.Errorf("name = %v", got)
	}
	for _, where := range [][]any{{"metadata", "labels"}, {"spec", "template", "metadata", "labels"}} {
		labels := dig(t, job, where...).(map[string]any)
		want := map[string]any{
			labelTask: "run_abc.2", sandbox.LabelRun: "run_abc", sandbox.LabelManagedBy: sandbox.ManagedByValue,
			"team": "ops", "custom": "yes",
		}
		if !reflect.DeepEqual(labels, want) {
			t.Errorf("labels at %v = %v, want %v", where, labels, want)
		}
	}
	for _, where := range [][]any{{"metadata", "annotations"}, {"spec", "template", "metadata", "annotations"}} {
		ann := dig(t, job, where...).(map[string]any)
		if ann[annTaskID] != "run_abc.2" || ann["note"] != "x" {
			t.Errorf("annotations at %v = %v", where, ann)
		}
	}
	spec := dig(t, job, "spec").(map[string]any)
	if spec["backoffLimit"] != float64(0) {
		t.Errorf("backoffLimit = %v", spec["backoffLimit"])
	}
	if spec["activeDeadlineSeconds"] != float64(3600+90) {
		t.Errorf("activeDeadlineSeconds = %v", spec["activeDeadlineSeconds"])
	}
	if spec["ttlSecondsAfterFinished"] != float64(600) {
		t.Errorf("ttl = %v", spec["ttlSecondsAfterFinished"])
	}

	pod := dig(t, job, "spec", "template", "spec").(map[string]any)
	if pod["restartPolicy"] != "Never" || pod["terminationGracePeriodSeconds"] != float64(90) {
		t.Errorf("restart %v grace %v", pod["restartPolicy"], pod["terminationGracePeriodSeconds"])
	}
	wantSC := map[string]any{"runAsNonRoot": true, "runAsUser": float64(1000), "runAsGroup": float64(1000), "fsGroup": float64(2000)}
	if !reflect.DeepEqual(pod["securityContext"], wantSC) {
		t.Errorf("securityContext = %v, want %v", pod["securityContext"], wantSC)
	}
	// Template fields stay.
	if dig(t, pod, "nodeSelector", "disk") != "ssd" || dig(t, pod, "tolerations", 0, "key") != "gpu" {
		t.Errorf("template fields lost: %v", pod)
	}
	secrets := dig(t, pod, "imagePullSecrets").([]any)
	if len(secrets) != 2 || dig(t, secrets, 0, "name") != "from-template" || dig(t, secrets, 1, "name") != "from-options" {
		t.Errorf("imagePullSecrets = %v", secrets)
	}

	containers := pod["containers"].([]any)
	if len(containers) != 2 || dig(t, containers, 1, "name") != "sidecar" {
		t.Fatalf("containers = %v", containers)
	}
	tc := containers[0].(map[string]any)
	if tc["name"] != "sandbox" || tc["image"] != task.Image || tc["imagePullPolicy"] != "Always" || tc["workingDir"] != "/workspace" {
		t.Errorf("task container = %v", tc)
	}
	if !reflect.DeepEqual(tc["command"], []any{"/opt/openfactory/openfactory", "runner"}) {
		t.Errorf("command = %v", tc["command"])
	}
	// Env is sorted by name.
	var names []string
	for _, e := range tc["env"].([]any) {
		names = append(names, e.(map[string]any)["name"].(string))
	}
	wantNames := []string{"EXTRA", "FROM_TEMPLATE", "HOME", "OF_BOOTSTRAP_TOKEN", "OF_CONTROL_URL",
		"OF_RUNNER_STATE_DIR", "OF_RUN_ID", "OF_SESSION", "OF_WORKSPACE"}
	if !reflect.DeepEqual(names, wantNames) {
		t.Errorf("env names = %v, want %v", names, wantNames)
	}
	env := envMap(t, tc)
	for k, v := range map[string]string{
		"OF_WORKSPACE": "/workspace", "OF_RUNNER_STATE_DIR": "/var/lib/openfactory/state", "HOME": "/home/agent",
		"OF_CONTROL_URL": "https://factory.internal", "OF_RUN_ID": "run_abc", "OF_SESSION": "2",
		"OF_BOOTSTRAP_TOKEN": "ofb_secret", "EXTRA": "1", "FROM_TEMPLATE": "yes",
	} {
		if env[k] != v {
			t.Errorf("env %s = %q, want %q", k, env[k], v)
		}
	}
	wantRes := map[string]any{
		"requests": map[string]any{"cpu": "2", "memory": "8Gi"},
		"limits":   map[string]any{"cpu": "2", "memory": "8Gi"},
	}
	if !reflect.DeepEqual(tc["resources"], wantRes) {
		t.Errorf("resources = %v", tc["resources"])
	}

	mounts := map[string]string{}
	for _, m := range tc["volumeMounts"].([]any) {
		mounts[m.(map[string]any)["name"].(string)] = m.(map[string]any)["mountPath"].(string)
	}
	wantMounts := map[string]string{"cache": "/cache", "workspace": "/workspace", "state": "/var/lib/openfactory",
		"home": "/home/agent", "runner": "/opt/openfactory"}
	if !reflect.DeepEqual(mounts, wantMounts) {
		t.Errorf("mounts = %v", mounts)
	}
	vols := map[string]any{}
	for _, v := range pod["volumes"].([]any) {
		vols[v.(map[string]any)["name"].(string)] = v.(map[string]any)["emptyDir"]
	}
	if len(vols) != 5 || !reflect.DeepEqual(vols["workspace"], map[string]any{"sizeLimit": "20Gi"}) {
		t.Errorf("volumes = %v", vols)
	}

	init := dig(t, pod, "initContainers", 0).(map[string]any)
	if init["name"] != "runner-install" || init["image"] != "example.test/runner:1" ||
		!reflect.DeepEqual(init["command"], []any{"cp", "/usr/local/bin/openfactory", "/opt/openfactory/openfactory"}) {
		t.Errorf("init container = %v", init)
	}
	if dig(t, init, "securityContext", "runAsUser") != float64(1000) || dig(t, init, "securityContext", "runAsNonRoot") != true {
		t.Errorf("init security context = %v", init["securityContext"])
	}
	if dig(t, init, "volumeMounts", 0, "name") != "runner" {
		t.Errorf("init mounts = %v", init["volumeMounts"])
	}
}

func TestStartWithoutRunnerImage(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_x", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	pod := dig(t, stored(t, f, task.TaskID), "spec", "template", "spec").(map[string]any)
	tc := dig(t, pod, "containers", 0).(map[string]any)
	if !reflect.DeepEqual(tc["command"], []any{"openfactory", "runner"}) {
		t.Errorf("command = %v", tc["command"])
	}
	if pod["initContainers"] != nil {
		t.Errorf("init containers = %v", pod["initContainers"])
	}
	if len(pod["volumes"].([]any)) != 3 {
		t.Errorf("volumes = %v", pod["volumes"])
	}
	// Defaults: retention 1 hour, user 1000, no resources.
	if dig(t, stored(t, f, task.TaskID), "spec", "ttlSecondsAfterFinished") != float64(3600) {
		t.Errorf("ttl default")
	}
	if tc["resources"] != nil || tc["imagePullPolicy"] != "IfNotPresent" {
		t.Errorf("task container = %v", tc)
	}
	// A task runner image wins over the option.
	b2 := newBackend(t, newFakeAPI(t), func(o *Options) { o.RunnerImage = "example.test/default:1" })
	m := b2.jobManifest(func() core.Task { x := testTask("run_y", 1); x.RunnerImage = "example.test/task:1"; return x }())
	init := dig(t, m, "spec", "template", "spec", "initContainers", 0).(map[string]any)
	if init["image"] != "example.test/task:1" {
		t.Errorf("runner image = %v", init["image"])
	}
}

func TestStartTemplateDoesNotChange(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.PodTemplate = "containers:\n- name: sandbox\n" })
	before, _ := json.Marshal(b.template)
	for i := 1; i <= 2; i++ {
		if _, err := b.Start(ctx5(t), testTask("run_t", i)); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := json.Marshal(b.template)
	if string(before) != string(after) {
		t.Fatalf("template changed: %s -> %s", before, after)
	}
}

func TestStartTwice(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_abc", 1)
	s1, err := b.Start(ctx5(t), task)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := b.Start(ctx5(t), task)
	if err != nil {
		t.Fatal(err)
	}
	if f.jobCount() != 1 {
		t.Fatalf("jobs = %d", f.jobCount())
	}
	if s1.State != core.TaskStarting || s2.State != core.TaskStarting || s1.TaskID != s2.TaskID {
		t.Fatalf("statuses %+v %+v", s1, s2)
	}
}

func TestStartInvalid(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	before := f.callCount()
	task := testTask("run_abc", 1)
	task.Image = ""
	_, err := b.Start(ctx5(t), task)
	if !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("err = %v", err)
	}
	if f.callCount() != before || f.jobCount() != 0 {
		t.Fatal("an invalid task caused an API call")
	}
}

func TestStartErrors(t *testing.T) {
	f := newFakeAPI(t)
	cfg := startFake(t, f)
	cfg.Token = "wrong"
	b, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.Start(ctx5(t), testTask("run_abc", 1))
	if err == nil || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewOptions(t *testing.T) {
	cfg := startFake(t, newFakeAPI(t))
	if _, err := New(Options{Config: cfg, ImagePullPolicy: "Sometimes"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("pull policy: %v", err)
	}
	if _, err := New(Options{Config: cfg, PodTemplate: "containers: [unclosed"}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("template: %v", err)
	}
	b, err := New(Options{Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if b.Namespace() != "agents" || b.Name() != "kubernetes" || b.opts.StopGrace != 2*time.Minute ||
		b.opts.ImagePullTimeout != 5*time.Minute || b.opts.UnschedulableTimeout != 10*time.Minute ||
		b.opts.PollInterval != 2*time.Second || b.opts.RunAsUser != 1000 {
		t.Fatalf("defaults: %+v", b.opts)
	}
	cfg.Namespace = ""
	if b, _ := New(Options{Config: cfg}); b.Namespace() != "default" {
		t.Fatalf("namespace = %q", b.Namespace())
	}
}

// --- mapStatus ---

var mapNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

func ts(d time.Duration) string { return mapNow.Add(d).Format(time.RFC3339) }

func decodeJob(t *testing.T, s string) jobObj {
	t.Helper()
	var j jobObj
	if err := json.Unmarshal([]byte(s), &j); err != nil {
		t.Fatal(err)
	}
	return j
}

func decodePod(t *testing.T, s string) podObj {
	t.Helper()
	var p podObj
	if err := json.Unmarshal([]byte(s), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMapStatus(t *testing.T) {
	opts := Options{ImagePullTimeout: 5 * time.Minute, UnschedulableTimeout: 10 * time.Minute}
	jobPlain := `{"metadata":{"annotations":{"openfactory.dev/sandbox-id":"run_a.1"}}}`
	jobCond := func(typ, reason string) string {
		return fmt.Sprintf(`{"metadata":{"annotations":{"openfactory.dev/sandbox-id":"run_a.1"}},
			"status":{"conditions":[{"type":%q,"status":"True","reason":%q,"message":"job msg","lastTransitionTime":%q}],
			"completionTime":%q}}`, typ, reason, ts(-time.Minute), ts(-time.Minute))
	}
	jobCancelled := `{"metadata":{"annotations":{"openfactory.dev/sandbox-id":"run_a.1","openfactory.dev/cancelled":"true"}}}`
	// A Job that started 25 minutes ago. The Pod of taskTerm ended 2 minutes
	// ago, so a deadline of 23 minutes ended it, and one of 60 minutes did not.
	jobDeadline := func(seconds int) string {
		return fmt.Sprintf(`{"metadata":{"annotations":{"openfactory.dev/sandbox-id":"run_a.1"}},
			"spec":{"activeDeadlineSeconds":%d},"status":{"startTime":%q}}`, seconds, ts(-25*time.Minute))
	}
	pod := func(created time.Duration, status string) string {
		return fmt.Sprintf(`{"metadata":{"name":"pod-1","creationTimestamp":%q},"status":%s}`, ts(created), status)
	}
	taskTerm := func(code int, reason, msg string) string {
		return fmt.Sprintf(`{"phase":"Failed","startTime":%q,"containerStatuses":[{"name":"sandbox","state":{"terminated":
			{"exitCode":%d,"reason":%q,"message":%q,"finishedAt":%q}}}]}`, ts(-20*time.Minute), code, reason, msg, ts(-2*time.Minute))
	}
	waiting := func(reason string) string {
		return fmt.Sprintf(`{"phase":"Pending","containerStatuses":[{"name":"sandbox","state":{"waiting":{"reason":%q,"message":"wait msg"}}}]}`, reason)
	}
	unsched := func(since time.Duration) string {
		return fmt.Sprintf(`{"phase":"Pending","conditions":[{"type":"PodScheduled","status":"False","reason":"Unschedulable",
			"message":"no nodes","lastTransitionTime":%q}]}`, ts(since))
	}
	long := strings.Repeat("x", 5000)

	code := func(n int) *int { return &n }
	tests := []struct {
		name    string
		job     string
		pods    []string
		state   core.TaskState
		reason  core.SandboxReason
		exit    *int
		message string
		sandbox string
	}{
		{name: "no pod yet", job: jobPlain, state: core.TaskStarting},
		{name: "pending pod", job: jobPlain, pods: []string{pod(-time.Minute, `{"phase":"Pending"}`)},
			state: core.TaskStarting, sandbox: "pod-1"},
		{name: "running pod", job: jobPlain, pods: []string{pod(-time.Minute, `{"phase":"Running","startTime":"`+ts(-30*time.Second)+`"}`)},
			state: core.TaskRunning, sandbox: "pod-1"},
		{name: "job complete", job: jobCond("Complete", ""), pods: []string{pod(-time.Hour, `{"phase":"Succeeded"}`)},
			state: core.TaskExited, reason: core.SandboxCompleted, exit: code(0), sandbox: "pod-1"},
		{name: "cancelled, pod gone", job: jobCancelled, state: core.TaskExited, reason: core.SandboxCancelled},
		{name: "cancelled, pod ended", job: jobCancelled, pods: []string{pod(-time.Hour, taskTerm(143, "Error", ""))},
			state: core.TaskExited, reason: core.SandboxCancelled, exit: code(143), sandbox: "pod-1"},
		{name: "cancelled, pod still running", job: jobCancelled, pods: []string{pod(-time.Hour, `{"phase":"Running"}`)},
			state: core.TaskRunning, sandbox: "pod-1"},
		{name: "deadline", job: jobCond("Failed", "DeadlineExceeded"), pods: []string{pod(-time.Hour, `{"phase":"Running"}`)},
			state: core.TaskExited, reason: core.SandboxDeadlineExceeded, message: "job msg", sandbox: "pod-1"},
		// The Job controller ends the Pod at the deadline before it writes the
		// condition Failed. The status must not say process_failed in between.
		{name: "deadline, Pod killed before the condition", job: jobDeadline(23 * 60), pods: []string{pod(-time.Hour, taskTerm(143, "Error", ""))},
			state: core.TaskExited, reason: core.SandboxDeadlineExceeded, exit: code(143),
			message: "Job was active longer than specified deadline", sandbox: "pod-1"},
		{name: "failed long before the deadline", job: jobDeadline(60 * 60), pods: []string{pod(-time.Hour, taskTerm(143, "Error", ""))},
			state: core.TaskExited, reason: core.SandboxProcessFailed, exit: code(143), sandbox: "pod-1"},
		{name: "evicted", job: jobPlain, pods: []string{pod(-time.Hour, `{"phase":"Failed","reason":"Evicted","message":"low disk"}`)},
			state: core.TaskExited, reason: core.SandboxEvicted, message: "low disk", sandbox: "pod-1"},
		{name: "oom", job: jobPlain, pods: []string{pod(-time.Hour, taskTerm(137, "OOMKilled", ""))},
			state: core.TaskExited, reason: core.SandboxContainerOOM, exit: code(137), sandbox: "pod-1"},
		{name: "process failed", job: jobPlain, pods: []string{pod(-time.Hour, taskTerm(2, "Error", "boom"))},
			state: core.TaskExited, reason: core.SandboxProcessFailed, exit: code(2), message: "boom", sandbox: "pod-1"},
		{name: "process failed, long message", job: jobPlain, pods: []string{pod(-time.Hour, taskTerm(1, "Error", long))},
			state: core.TaskExited, reason: core.SandboxProcessFailed, exit: code(1), message: long[:2048], sandbox: "pod-1"},
		{name: "exit 0 before the job condition", job: jobPlain, pods: []string{pod(-time.Hour, taskTerm(0, "Completed", ""))},
			state: core.TaskExited, reason: core.SandboxCompleted, exit: code(0), sandbox: "pod-1"},
		{name: "image pull, before timeout", job: jobPlain, pods: []string{pod(-4*time.Minute, waiting("ImagePullBackOff"))},
			state: core.TaskStarting, sandbox: "pod-1"},
		{name: "image pull, ErrImagePull after timeout", job: jobPlain, pods: []string{pod(-6*time.Minute, waiting("ErrImagePull"))},
			state: core.TaskExited, reason: core.SandboxImagePull, message: "ErrImagePull: wait msg", sandbox: "pod-1"},
		{name: "image pull, ImagePullBackOff after timeout", job: jobPlain, pods: []string{pod(-6*time.Minute, waiting("ImagePullBackOff"))},
			state: core.TaskExited, reason: core.SandboxImagePull, message: "ImagePullBackOff: wait msg", sandbox: "pod-1"},
		{name: "invalid image name after timeout", job: jobPlain, pods: []string{pod(-6*time.Minute, waiting("InvalidImageName"))},
			state: core.TaskExited, reason: core.SandboxImagePull, message: "InvalidImageName: wait msg", sandbox: "pod-1"},
		{name: "runner image pull after timeout", job: jobPlain, pods: []string{pod(-6*time.Minute,
			`{"phase":"Pending","initContainerStatuses":[{"name":"runner-install","state":{"waiting":{"reason":"ImagePullBackOff","message":"wait msg"}}}]}`)},
			state: core.TaskExited, reason: core.SandboxImagePull, message: "ImagePullBackOff: wait msg", sandbox: "pod-1"},
		// The wait for the NetworkPolicy ends with exit code 75 when the policy
		// is not active in time. The task command never ran.
		{name: "network policy wait timed out", job: jobPlain, pods: []string{pod(-time.Hour,
			`{"phase":"Failed","initContainerStatuses":[{"name":"egress-wait","state":{"terminated":{"exitCode":75,"reason":"Error","message":"wait msg"}}}]}`)},
			state: core.TaskExited, reason: core.SandboxNetworkPolicyTimeout, exit: code(75),
			message: "init container egress-wait: wait msg", sandbox: "pod-1"},
		{name: "network policy wait crashed", job: jobPlain, pods: []string{pod(-time.Hour,
			`{"phase":"Failed","initContainerStatuses":[{"name":"egress-wait","state":{"terminated":{"exitCode":1,"reason":"Error","message":"unknown command"}}}]}`)},
			state: core.TaskExited, reason: core.SandboxProcessFailed, exit: code(1),
			message: "init container egress-wait: unknown command", sandbox: "pod-1"},
		{name: "exit 75 of another init container", job: jobPlain, pods: []string{pod(-time.Hour,
			`{"phase":"Failed","initContainerStatuses":[{"name":"runner-install","state":{"terminated":{"exitCode":75,"reason":"Error","message":"x"}}}]}`)},
			state: core.TaskExited, reason: core.SandboxProcessFailed, exit: code(75),
			message: "init container runner-install: x", sandbox: "pod-1"},
		{name: "config error", job: jobPlain, pods: []string{pod(-time.Second, waiting("CreateContainerConfigError"))},
			state: core.TaskExited, reason: core.SandboxInvalidSpec, message: "wait msg", sandbox: "pod-1"},
		{name: "unschedulable, before timeout", job: jobPlain, pods: []string{pod(-time.Hour, unsched(-9*time.Minute))},
			state: core.TaskStarting, sandbox: "pod-1"},
		{name: "unschedulable, after timeout", job: jobPlain, pods: []string{pod(-time.Hour, unsched(-11*time.Minute))},
			state: core.TaskExited, reason: core.SandboxUnschedulable, message: "no nodes", sandbox: "pod-1"},
		{name: "node lost", job: jobPlain, pods: []string{pod(-time.Hour, `{"phase":"Failed","reason":"NodeLost"}`)},
			state: core.TaskExited, reason: core.SandboxNodeLost, sandbox: "pod-1"},
		{name: "admission error", job: jobPlain, pods: []string{pod(-time.Hour, `{"phase":"Failed","reason":"UnexpectedAdmissionError"}`)},
			state: core.TaskExited, reason: core.SandboxNodeLost, sandbox: "pod-1"},
		{name: "job failed, no pod info", job: jobCond("Failed", "BackoffLimitExceeded"), pods: []string{pod(-time.Hour, `{"phase":"Failed"}`)},
			state: core.TaskExited, reason: core.SandboxProcessFailed, message: "job msg", sandbox: "pod-1"},
		{name: "job failed, no pod", job: jobCond("Failed", "BackoffLimitExceeded"),
			state: core.TaskExited, reason: core.SandboxUnknown, message: "job msg"},
		{name: "job failed, unknown pod reason", job: jobCond("Failed", "BackoffLimitExceeded"),
			pods:  []string{pod(-time.Hour, `{"phase":"Failed","reason":"Shutdown"}`)},
			state: core.TaskExited, reason: core.SandboxUnknown, message: "job msg", sandbox: "pod-1"},
		{name: "newest pod wins", job: jobPlain, pods: []string{
			`{"metadata":{"name":"old","creationTimestamp":"` + ts(-time.Hour) + `"},"status":` + taskTerm(1, "Error", "") + `}`,
			`{"metadata":{"name":"new","creationTimestamp":"` + ts(-time.Minute) + `"},"status":{"phase":"Running"}}`},
			state: core.TaskRunning, sandbox: "new"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var pods []podObj
			for _, p := range tc.pods {
				pods = append(pods, decodePod(t, p))
			}
			got := mapStatus(decodeJob(t, tc.job), pods, mapNow, opts)
			if got.TaskID != "run_a.1" || got.State != tc.state || got.Reason != tc.reason {
				t.Fatalf("got %+v", got)
			}
			if (got.ExitCode == nil) != (tc.exit == nil) || (tc.exit != nil && *got.ExitCode != *tc.exit) {
				t.Errorf("exit = %v, want %v", got.ExitCode, tc.exit)
			}
			if got.Message != tc.message {
				t.Errorf("message = %q, want %q", got.Message, tc.message)
			}
			if got.SandboxID != tc.sandbox {
				t.Errorf("sandbox = %q, want %q", got.SandboxID, tc.sandbox)
			}
			if got.State == core.TaskExited && tc.reason != core.SandboxImagePull && tc.reason != core.SandboxUnschedulable &&
				tc.reason != core.SandboxInvalidSpec && got.EndedAt == nil && (tc.name == "job complete" || tc.name == "deadline") {
				t.Errorf("no EndedAt")
			}
		})
	}

	t.Run("times", func(t *testing.T) {
		got := mapStatus(decodeJob(t, jobCond("Complete", "")),
			[]podObj{decodePod(t, pod(-time.Hour, `{"phase":"Succeeded","startTime":"`+ts(-50*time.Minute)+`"}`))}, mapNow, opts)
		if got.StartedAt == nil || !got.StartedAt.Equal(mapNow.Add(-50*time.Minute)) {
			t.Errorf("StartedAt = %v", got.StartedAt)
		}
		if got.EndedAt == nil || !got.EndedAt.Equal(mapNow.Add(-time.Minute)) {
			t.Errorf("EndedAt = %v", got.EndedAt)
		}
	})
}

// --- Status, Watch, Cancel, Destroy, List, Preflight ---

func TestStatusUnknownTask(t *testing.T) {
	b := newBackend(t, newFakeAPI(t), nil)
	if _, err := b.Status(ctx5(t), "run_none.1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := b.Watch(ctx5(t), "run_none.1"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("watch err = %v", err)
	}
	if err := b.Cancel(ctx5(t), "run_none.1", 5); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cancel err = %v", err)
	}
}

func TestStatusFromAPI(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	// A Pod of another task must not count.
	f.addPod("run_other.1", "other-pod", time.Now(), map[string]any{"phase": "Running"})
	st, err := b.Status(ctx5(t), task.TaskID)
	if err != nil || st.State != core.TaskStarting || st.SandboxID != "" {
		t.Fatalf("status = %+v, err %v", st, err)
	}
	f.addPod(task.TaskID, "pod-a", time.Now(), map[string]any{"phase": "Running"})
	if st, _ = b.Status(ctx5(t), task.TaskID); st.State != core.TaskRunning || st.SandboxID != "pod-a" || st.TaskID != task.TaskID {
		t.Fatalf("status = %+v", st)
	}
}

func recv(t *testing.T, ch <-chan core.TaskStatus) (core.TaskStatus, bool) {
	t.Helper()
	select {
	case st, ok := <-ch:
		return st, ok
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for a status")
		return core.TaskStatus{}, false
	}
}

func TestWatch(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	f.addPod(task.TaskID, "pod-a", time.Now(), map[string]any{"phase": "Pending"})
	ch, err := b.Watch(ctx5(t), task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if st, ok := recv(t, ch); !ok || st.State != core.TaskStarting {
		t.Fatalf("first = %+v %v", st, ok)
	}
	f.setPodStatus("pod-a", map[string]any{"phase": "Running"})
	if st, ok := recv(t, ch); !ok || st.State != core.TaskRunning {
		t.Fatalf("second = %+v %v", st, ok)
	}
	f.setPodStatus("pod-a", map[string]any{"phase": "Succeeded"})
	f.setJobStatus(task.TaskID, map[string]any{
		"conditions": []any{map[string]any{"type": "Complete", "status": "True"}},
	})
	st, ok := recv(t, ch)
	if !ok || st.State != core.TaskExited || st.Reason != core.SandboxCompleted || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("third = %+v %v", st, ok)
	}
	if _, ok := recv(t, ch); ok {
		t.Fatal("channel not closed after exited")
	}
}

func TestWatchEndsWithContext(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := b.Watch(ctx, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := recv(t, ch); !ok {
		t.Fatal("no first status")
	}
	cancel()
	if _, ok := recv(t, ch); ok {
		t.Fatal("channel not closed after ctx ended")
	}
}

func TestWatchJobGone(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	ch, err := b.Watch(ctx5(t), task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := recv(t, ch); st.State != core.TaskStarting {
		t.Fatalf("first = %+v", st)
	}
	if err := b.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatal(err)
	}
	st, ok := recv(t, ch)
	if !ok || st.State != core.TaskExited || st.Reason != core.SandboxUnknown || st.TaskID != task.TaskID {
		t.Fatalf("got %+v %v", st, ok)
	}
	if _, ok := recv(t, ch); ok {
		t.Fatal("channel not closed")
	}
}

func TestCancel(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.StopGrace = 45 * time.Second })
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	f.addPod(task.TaskID, "pod-a", time.Now(), map[string]any{"phase": "Running"})
	f.addPod("run_other.1", "pod-other", time.Now(), map[string]any{"phase": "Running"})

	if err := b.Cancel(ctx5(t), task.TaskID, 30); err != nil {
		t.Fatal(err)
	}
	if f.patchType != "application/merge-patch+json" {
		t.Errorf("patch content type = %q", f.patchType)
	}
	if got := dig(t, stored(t, f, task.TaskID), "metadata", "annotations", annCancelled); got != "true" {
		t.Errorf("annotation = %v", got)
	}
	if len(f.podDeletes) != 1 || f.podDeletes[0]["pod"] != "pod-a" || f.podDeletes[0]["gracePeriodSeconds"] != float64(30) {
		t.Errorf("pod deletes = %v", f.podDeletes)
	}
	st, err := b.Status(ctx5(t), task.TaskID)
	if err != nil || st.State != core.TaskExited || st.Reason != core.SandboxCancelled {
		t.Fatalf("status = %+v err %v", st, err)
	}

	// Grace 0 means StopGrace. A second cancel is fine.
	f.addPod(task.TaskID, "pod-b", time.Now(), map[string]any{"phase": "Running"})
	if err := b.Cancel(ctx5(t), task.TaskID, 0); err != nil {
		t.Fatal(err)
	}
	if n := len(f.podDeletes); n != 2 || f.podDeletes[1]["gracePeriodSeconds"] != float64(45) {
		t.Errorf("pod deletes = %v", f.podDeletes)
	}
}

func TestDestroy(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	if err := b.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatal(err)
	}
	if f.jobCount() != 0 {
		t.Fatal("job not deleted")
	}
	if f.deleteBody["propagationPolicy"] != "Background" || f.deleteBody["kind"] != "DeleteOptions" {
		t.Errorf("delete body = %v", f.deleteBody)
	}
	if err := b.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatalf("second destroy: %v", err)
	}
	if _, err := b.Status(ctx5(t), task.TaskID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("status after destroy: %v", err)
	}
}

func TestDestroyNoCleanup(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, func(o *Options) { o.NoCleanup = true })
	task := testTask("run_abc", 1)
	if _, err := b.Start(ctx5(t), task); err != nil {
		t.Fatal(err)
	}
	if err := b.Destroy(ctx5(t), task.TaskID); err != nil {
		t.Fatal(err)
	}
	if f.jobCount() != 1 {
		t.Fatal("job deleted with NoCleanup")
	}
}

func TestList(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	for _, task := range []core.Task{testTask("run_b", 1), testTask("run_a", 2)} {
		if _, err := b.Start(ctx5(t), task); err != nil {
			t.Fatal(err)
		}
	}
	f.addPod("run_a.2", "pod-a", time.Now(), map[string]any{"phase": "Running"})
	// A Job that Open Factory does not manage is not listed.
	f.mu.Lock()
	f.jobs["foreign"] = map[string]any{"metadata": map[string]any{"name": "foreign",
		"labels": map[string]any{"app": "other"}}}
	f.jobs["half"] = map[string]any{"metadata": map[string]any{"name": "half",
		"labels": map[string]any{sandbox.LabelManagedBy: sandbox.ManagedByValue}}}
	f.mu.Unlock()

	list, err := b.List(ctx5(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].TaskID != "run_a.2" || list[1].TaskID != "run_b.1" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].State != core.TaskRunning || list[0].SandboxID != "pod-a" || list[1].State != core.TaskStarting {
		t.Fatalf("list = %+v", list)
	}
}

func TestPreflight(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	if err := b.Preflight(ctx5(t)); err != nil {
		t.Fatal(err)
	}
	f.forbidJobs = true
	err := b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "jobs") || !strings.Contains(err.Error(), "permission") ||
		!strings.Contains(err.Error(), "agents") {
		t.Fatalf("err = %v", err)
	}
	f.forbidJobs = false
	f.forbidPodsL = true
	err = b.Preflight(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "pods") || !strings.Contains(err.Error(), "permission") {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIErrorMessage(t *testing.T) {
	f := newFakeAPI(t)
	b := newBackend(t, f, nil)
	f.forbidJobs = true
	_, err := b.List(ctx5(t))
	if err == nil || !strings.Contains(err.Error(), "User cannot list resource") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("err = %v", err)
	}
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != http.StatusForbidden {
		t.Fatalf("err = %v", err)
	}
}
