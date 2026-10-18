// Package k8stask implements core.TaskBackend with one Kubernetes Job for each
// task. The Job has backoffLimit 0 and one Pod. The backend talks to the
// Kubernetes API over HTTPS with the helpers of package kubernetes (no
// client-go).
//
// The Pod runs the runner (`openfactory runner`) in the container "sandbox". The
// image of the task is Task.Image. The runner binary comes from one of two
// places:
//
//   - The task image already holds `openfactory` in its PATH. Then no runner
//     image is set, and the command is ["openfactory", "runner"].
//   - A runner image is set (Task.RunnerImage, else Options.RunnerImage). It
//     must hold the binary at /usr/local/bin/openfactory. An init container
//     "runner-install" copies it to /opt/openfactory/openfactory in an emptyDir,
//     and the command is ["/opt/openfactory/openfactory", "runner"]. The image
//     needs the program `cp`.
//
// Each entry of Task.ToolImages gets an init container "tool-<name>" that copies
// /opt/openfactory/tool/ of the tool image to /opt/openfactory/tools/<name>.
// The command of the task is then a shell that puts
// /opt/openfactory/tools/<name>/bin in front of the PATH of the task image and
// starts the runner.
//
// The workspace, the runner state and the home directory are emptyDir volumes.
// The Pod runs as a numeric user that is not root (Options.RunAsUser).
//
// A task with a restricted network policy gets a NetworkPolicy (netpol.go). The
// Preflight method checks the permissions and, with Options.PreflightJob, runs a
// test Job (preflight.go).
//
// Status is computed from the Job and its Pods each time it is asked. The
// backend keeps no state in memory, so a restart of the server loses nothing.
package k8stask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
)

var _ core.TaskBackend = (*Backend)(nil)

// BackendName is the name of the backend.
const BackendName = "kubernetes"

const (
	labelTask    = "openfactory.dev/sandbox"
	annTaskID    = "openfactory.dev/sandbox-id"
	annCancelled = "openfactory.dev/cancelled"

	watchBuffer = 8
)

// Options configures the backend.
type Options struct {
	// Config is the connection to the cluster. If nil, the backend calls
	// kubernetes.LoadConfig(Kubeconfig, Context).
	Config     *kubernetes.RestConfig
	Kubeconfig string
	Context    string
	// Namespace of the Jobs. Default: the namespace of the config, else "default".
	Namespace string
	// PodTemplate is a raw PodSpec in YAML or JSON.
	PodTemplate string
	// ImagePullPolicy of the containers. Default IfNotPresent.
	ImagePullPolicy  string
	ImagePullSecrets []string
	ExtraLabels      map[string]string
	ExtraAnnotations map[string]string
	// RunnerImage is the runner image for a task that has none. It can be empty.
	RunnerImage string
	// PreflightToolImages are copied by the init containers of the preflight
	// Job, so that the Job tests the copy as the real tasks do.
	PreflightToolImages []core.TaskImage
	// RunAsUser is the numeric user of the Pod. Default 1000.
	RunAsUser int64
	// RunAsImageUser keeps the user of the image. The backend then sets no
	// runAsUser, runAsGroup, fsGroup or runAsNonRoot in the Pod. It is not
	// valid with the egress lock.
	RunAsImageUser bool
	// ActiveDeadline is an upper limit of activeDeadlineSeconds of the Job.
	// The Job deadline is the session time plus the stop grace time, or this
	// value if that is shorter. 0 means no extra limit.
	ActiveDeadline time.Duration
	// WorkspaceSizeLimit is the sizeLimit of the workspace volume for a task
	// that sets none. Empty means no limit.
	WorkspaceSizeLimit string
	// ControlPlaneNamespace and ControlPlaneSelector select the Pods of the
	// control plane that a restricted task can reach. The selector is a
	// label selector string such as "app.kubernetes.io/name=openfactory".
	// Without a selector, a restricted task can reach the control URL only
	// if its host is an IP address.
	ControlPlaneNamespace string
	ControlPlaneSelector  string
	// EgressProxySidecarImage is an image that holds the openfactory binary.
	// If set, a restricted task gets a netguard sidecar with the allowed
	// hosts of the task. Without it, a restricted task reaches only DNS and
	// the control plane.
	EgressProxySidecarImage string
	// EgressLock chooses how the backend closes the direct path on ports 80
	// and 443 next to the sidecar: "none" (default) or "iptables" (package
	// egress). It needs EgressProxySidecarImage.
	EgressLock string
	// EgressWaitImage is the image of the init container that holds the task
	// back until its NetworkPolicy is active (package egress, wait.go). It
	// must hold the openfactory binary. Default: the runner image, then the
	// proxy sidecar image, then the task image.
	EgressWaitImage string
	// EgressWaitTimeout is the time that this init container waits. 0 means 30
	// seconds. A negative value turns the wait off: a restricted task then
	// starts at once, and the NetworkPolicy takes effect a few hundred
	// milliseconds later. The wait is also off when AllowUnenforcedNetworkPolicy
	// is set, or when the check found that the cluster does not enforce
	// NetworkPolicy: the policy would never take effect.
	EgressWaitTimeout time.Duration
	// EgressWaitAddr is the host:port that the wait dials. It must accept
	// connections without a policy, and the policy of a task must block it.
	// Default: the kubelet of the node of the Pod (port 10250).
	EgressWaitAddr string
	// AllowUnenforcedNetworkPolicy lets a restricted task start when its
	// NetworkPolicy cannot be created. The backend then logs a warning.
	AllowUnenforcedNetworkPolicy bool
	// NetworkPolicyCheck is "off" (default) or "probe". With "probe",
	// Preflight starts a test in the background that finds out whether the
	// cluster enforces NetworkPolicy (egress.Probe).
	NetworkPolicyCheck string
	// NetworkPolicyEnforcement is "warn" (default) or "require". With
	// "require", a restricted task fails unless the check showed that the
	// cluster enforces NetworkPolicy.
	NetworkPolicyEnforcement string
	// NetworkPolicyProbeImage is the image of the probe Pods. It must hold the
	// openfactory binary. Default: EgressProxySidecarImage, then RunnerImage.
	NetworkPolicyProbeImage string
	// PreflightJob makes Preflight run a small test Job.
	PreflightJob bool
	// PreflightImage is the image of the test Job when no runner image is set.
	// Default: kubernetes.DefaultImage.
	PreflightImage string
	// PreflightTimeout is the time that the test Job has to end. Default 3 minutes.
	PreflightTimeout time.Duration
	// StopGrace is the grace time of the Pod and the margin of the deadline. Default 2 minutes.
	StopGrace time.Duration
	// FailureRetention is the time to live of a finished Job when the task sets none. Default 1 hour.
	FailureRetention time.Duration
	// ImagePullTimeout is the time after which an image pull error ends the task. Default 5 minutes.
	ImagePullTimeout time.Duration
	// UnschedulableTimeout is the time after which an unschedulable Pod ends the task. Default 10 minutes.
	UnschedulableTimeout time.Duration
	// PollInterval is the time between two Status calls of Watch. Default 2 seconds.
	PollInterval time.Duration
	// NoCleanup keeps the Job when Destroy is called. It is for debug.
	NoCleanup bool
	Logger    *slog.Logger
}

// Backend runs each task as a Kubernetes Job.
type Backend struct {
	opts     Options
	cfg      *kubernetes.RestConfig
	http     *http.Client
	ns       string
	template map[string]any
	log      *slog.Logger
	// netpol is the answer of Preflight to the question whether the backend
	// can manage NetworkPolicies: netpolUnknown, netpolAllowed or netpolDenied.
	netpol atomic.Int32
	// cp is the parsed control plane peer of the NetworkPolicy.
	cp peer
	// enforce checks whether the cluster enforces NetworkPolicy. It is nil
	// if the check is off or has no image.
	enforce *egress.Checker
}

const (
	netpolUnknown int32 = iota
	netpolAllowed
	netpolDenied
)

// New creates a backend and checks the options and the pod template.
func New(opts Options) (*Backend, error) {
	cfg := opts.Config
	if cfg == nil {
		var err error
		if cfg, err = kubernetes.LoadConfig(opts.Kubeconfig, opts.Context); err != nil {
			return nil, err
		}
	}
	ns := opts.Namespace
	if ns == "" {
		ns = cfg.Namespace
	}
	if ns == "" {
		ns = "default"
	}
	switch opts.ImagePullPolicy {
	case "":
		opts.ImagePullPolicy = "IfNotPresent"
	case "Always", "IfNotPresent", "Never":
	default:
		return nil, fmt.Errorf("%w: image pull policy must be Always, IfNotPresent or Never", core.ErrInvalid)
	}
	if opts.RunAsUser < 0 {
		return nil, fmt.Errorf("%w: run as user must be 1 or more", core.ErrInvalid)
	}
	if opts.RunAsUser == 0 {
		opts.RunAsUser = 1000
	}
	if opts.StopGrace <= 0 {
		opts.StopGrace = 2 * time.Minute
	}
	if opts.FailureRetention <= 0 {
		opts.FailureRetention = time.Hour
	}
	if opts.ImagePullTimeout <= 0 {
		opts.ImagePullTimeout = 5 * time.Minute
	}
	if opts.UnschedulableTimeout <= 0 {
		opts.UnschedulableTimeout = 10 * time.Minute
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 2 * time.Second
	}
	if opts.PreflightTimeout <= 0 {
		opts.PreflightTimeout = 3 * time.Minute
	}
	if opts.ActiveDeadline < 0 {
		return nil, fmt.Errorf("%w: active deadline must not be negative", core.ErrInvalid)
	}
	cp, err := newPeer("control plane", opts.ControlPlaneNamespace, opts.ControlPlaneSelector)
	if err != nil {
		return nil, err
	}
	for _, err := range []error{
		egress.ValidateLock(opts.EgressLock),
		egress.ValidateWaitAddr(opts.EgressWaitAddr),
		egress.ValidateCheck(opts.NetworkPolicyCheck),
		egress.ValidateEnforcement(opts.NetworkPolicyEnforcement),
	} {
		if err != nil {
			return nil, fmt.Errorf("%w: %w", core.ErrInvalid, err)
		}
	}
	if egress.LockEnabled(opts.EgressLock) && opts.EgressProxySidecarImage == "" {
		return nil, fmt.Errorf("%w: the egress lock needs the proxy sidecar: set the sidecar image", core.ErrInvalid)
	}
	if egress.LockEnabled(opts.EgressLock) && opts.RunAsImageUser {
		return nil, fmt.Errorf("%w: the egress lock needs a numeric sandbox user: turn off run as image user, or turn off the lock", core.ErrInvalid)
	}
	tmpl, err := kubernetes.ParsePodTemplate(opts.PodTemplate)
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	b := &Backend{
		opts: opts, cfg: cfg, http: cfg.HTTPClient(), ns: ns, template: tmpl,
		log: log.With("backend", BackendName), cp: cp,
	}
	b.initEnforcementCheck()
	return b, nil
}

// Name returns "kubernetes".
func (b *Backend) Name() string { return BackendName }

// Namespace returns the namespace of the Jobs.
func (b *Backend) Namespace() string { return b.ns }

// apiError is a response of the API that is not a success.
type apiError struct {
	Code    int
	Message string
}

func (e *apiError) Error() string {
	switch e.Code {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("kubernetes API: permission problem (HTTP %d): %s", e.Code, e.Message)
	}
	return fmt.Sprintf("kubernetes API: HTTP %d: %s", e.Code, e.Message)
}

func isCode(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == code
}

// do calls the API. A body that is not nil is sent as JSON, or with
// contentType when that is set. A 404 gives an error that wraps core.ErrNotFound.
func (b *Backend) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.cfg.Server+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		if contentType == "" {
			contentType = "application/json"
		}
		req.Header.Set("Content-Type", contentType)
	}
	if err := b.cfg.Authorize(req.Header); err != nil {
		return err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &st) == nil && st.Message != "" {
			msg = st.Message
		}
		e := &apiError{Code: resp.StatusCode, Message: msg}
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: %w", core.ErrNotFound, e)
		}
		return e
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (b *Backend) jobsPath() string {
	return "/apis/batch/v1/namespaces/" + url.PathEscape(b.ns) + "/jobs"
}

func (b *Backend) podsPath() string {
	return "/api/v1/namespaces/" + url.PathEscape(b.ns) + "/pods"
}

// Start creates the Job of the task. A second call for the same task ID
// returns the status of the existing Job.
func (b *Backend) Start(ctx context.Context, task core.Task) (core.TaskStatus, error) {
	if err := task.Validate(); err != nil {
		return core.TaskStatus{}, fmt.Errorf("k8stask: invalid task: %w: %w", core.ErrInvalid, err)
	}
	if err := validateToolImages(task.ToolImages); err != nil {
		return core.TaskStatus{}, fmt.Errorf("k8stask: invalid task: %w: %w", core.ErrInvalid, err)
	}
	if _, image := b.podUser(task); image && b.lockActive(task) {
		return core.TaskStatus{}, fmt.Errorf("k8stask: invalid task: %w: the egress lock needs a numeric user: platform.linux.user must not be \"image\"", core.ErrInvalid)
	}
	// The NetworkPolicy comes first, so that the Pod never runs before it.
	npCreated := false
	if restricted(task) {
		var err error
		if npCreated, err = b.ensureNetworkPolicy(ctx, task); err != nil {
			return core.TaskStatus{}, err
		}
	}
	job := b.jobManifest(task)
	var created struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	err := b.do(ctx, http.MethodPost, b.jobsPath(), "", job, &created)
	switch {
	case err == nil:
		if npCreated {
			if perr := b.ownNetworkPolicy(ctx, task.TaskID, created.Metadata.UID); perr != nil {
				b.discard(task.TaskID)
				return core.TaskStatus{}, perr
			}
		}
		b.log.Info("task job created", "task", task.TaskID, "job", jobName(task.TaskID), "namespace", b.ns)
		return core.TaskStatus{TaskID: task.TaskID, State: core.TaskStarting}, nil
	case isCode(err, http.StatusConflict):
		return b.Status(ctx, task.TaskID)
	}
	if npCreated {
		b.deleteNetworkPolicy(ctx, task.TaskID)
	}
	return core.TaskStatus{}, fmt.Errorf("k8stask: create job: %w", err)
}

func (b *Backend) getJob(ctx context.Context, taskID string) (jobObj, error) {
	var j jobObj
	if err := b.do(ctx, http.MethodGet, b.jobsPath()+"/"+url.PathEscape(jobName(taskID)), "", nil, &j); err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return j, fmt.Errorf("k8stask: task %q: %w", taskID, core.ErrNotFound)
		}
		return j, fmt.Errorf("k8stask: get job: %w", err)
	}
	return j, nil
}

func (b *Backend) listPods(ctx context.Context, selector, taskID string) ([]podObj, error) {
	var list struct {
		Items []podObj `json:"items"`
	}
	q := url.Values{"labelSelector": {selector}}
	if err := b.do(ctx, http.MethodGet, b.podsPath()+"?"+q.Encode(), "", nil, &list); err != nil {
		return nil, fmt.Errorf("k8stask: list pods: %w", err)
	}
	return filterPods(list.Items, taskID), nil
}

// filterPods keeps the Pods of one task. The label value can be a cut form of
// the ID, so the exact ID of the annotation decides.
func filterPods(pods []podObj, taskID string) []podObj {
	out := pods[:0:0]
	for _, p := range pods {
		if id := p.Metadata.Annotations[annTaskID]; id == "" || id == taskID {
			out = append(out, p)
		}
	}
	return out
}

// Status returns the status of the task. An unknown task gives core.ErrNotFound.
func (b *Backend) Status(ctx context.Context, taskID string) (core.TaskStatus, error) {
	j, err := b.getJob(ctx, taskID)
	if err != nil {
		return core.TaskStatus{}, err
	}
	pods, err := b.listPods(ctx, labelTask+"="+taskLabel(taskID), taskID)
	if err != nil {
		return core.TaskStatus{}, err
	}
	st := mapStatus(j, pods, time.Now(), b.opts)
	st.TaskID = taskID
	return st, nil
}

// Watch sends the current status and each change of state, reason or exit
// code. The channel closes after the status exited, when the task is gone, or
// when ctx ends. A task that is gone gives one exited status with the reason unknown.
func (b *Backend) Watch(ctx context.Context, taskID string) (<-chan core.TaskStatus, error) {
	first, err := b.Status(ctx, taskID)
	if err != nil {
		return nil, err
	}
	ch := make(chan core.TaskStatus, watchBuffer)
	go func() {
		defer close(ch)
		last := first
		push(ch, first)
		if first.Exited() {
			return
		}
		t := time.NewTicker(b.opts.PollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			st, err := b.Status(ctx, taskID)
			if errors.Is(err, core.ErrNotFound) {
				push(ch, core.TaskStatus{
					TaskID: taskID, State: core.TaskExited, Reason: core.SandboxUnknown,
					Message: "the Job of the task is gone",
				})
				return
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				b.log.Debug("watch: status failed", "task", taskID, "err", err)
				continue
			}
			if changed(last, st) {
				push(ch, st)
				last = st
			}
			if st.Exited() {
				return
			}
		}
	}()
	return ch, nil
}

// push sends a status. A full buffer drops its oldest status. Only one
// goroutine writes to the channel.
func push(ch chan core.TaskStatus, st core.TaskStatus) {
	for {
		select {
		case ch <- st:
			return
		default:
			select {
			case <-ch:
			default:
			}
		}
	}
}

func changed(a, b core.TaskStatus) bool {
	if a.State != b.State || a.Reason != b.Reason {
		return true
	}
	switch {
	case a.ExitCode == nil && b.ExitCode == nil:
		return false
	case a.ExitCode == nil || b.ExitCode == nil:
		return true
	}
	return *a.ExitCode != *b.ExitCode
}

// Cancel marks the Job as cancelled and deletes its Pods with the grace time.
// The runner gets SIGTERM and stops. A grace time of 0 or less means StopGrace.
func (b *Backend) Cancel(ctx context.Context, taskID string, graceSeconds int) error {
	patch := map[string]any{"metadata": map[string]any{"annotations": map[string]any{annCancelled: "true"}}}
	err := b.do(ctx, http.MethodPatch, b.jobsPath()+"/"+url.PathEscape(jobName(taskID)),
		"application/merge-patch+json", patch, nil)
	if errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("k8stask: task %q: %w", taskID, core.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("k8stask: mark job as cancelled: %w", err)
	}
	if graceSeconds <= 0 {
		graceSeconds = int(b.opts.StopGrace / time.Second)
	}
	pods, err := b.listPods(ctx, labelTask+"="+taskLabel(taskID), taskID)
	if err != nil {
		return err
	}
	for _, p := range pods {
		body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "gracePeriodSeconds": graceSeconds}
		err := b.do(ctx, http.MethodDelete, b.podsPath()+"/"+url.PathEscape(p.Metadata.Name), "", body, nil)
		if err != nil && !errors.Is(err, core.ErrNotFound) {
			return fmt.Errorf("k8stask: delete pod: %w", err)
		}
	}
	return nil
}

// Destroy deletes the Job and, in the background, its Pods. A missing Job is
// not an error. With NoCleanup it does nothing.
func (b *Backend) Destroy(ctx context.Context, taskID string) error {
	if b.opts.NoCleanup {
		b.log.Info("task job kept (no cleanup)", "task", taskID, "job", jobName(taskID))
		return nil
	}
	body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background"}
	err := b.do(ctx, http.MethodDelete, b.jobsPath()+"/"+url.PathEscape(jobName(taskID)), "", body, nil)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("k8stask: delete job: %w", err)
	}
	// The Job owns its NetworkPolicy, so the delete cascades. The explicit
	// delete covers a Job that is already gone.
	if err := b.deleteNetworkPolicyErr(ctx, taskID); err != nil {
		return err
	}
	return nil
}

// List returns the status of each task that has a Job, sorted by task ID.
func (b *Backend) List(ctx context.Context) ([]core.TaskStatus, error) {
	selector := sandbox.LabelManagedBy + "=" + sandbox.ManagedByValue + "," + labelTask
	var jobs struct {
		Items []jobObj `json:"items"`
	}
	q := url.Values{"labelSelector": {selector}}
	if err := b.do(ctx, http.MethodGet, b.jobsPath()+"?"+q.Encode(), "", nil, &jobs); err != nil {
		return nil, fmt.Errorf("k8stask: list jobs: %w", err)
	}
	// One call gets the Pods of all tasks.
	var pods struct {
		Items []podObj `json:"items"`
	}
	if err := b.do(ctx, http.MethodGet, b.podsPath()+"?"+q.Encode(), "", nil, &pods); err != nil {
		return nil, fmt.Errorf("k8stask: list pods: %w", err)
	}
	now := time.Now()
	out := make([]core.TaskStatus, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		id := j.Metadata.Annotations[annTaskID]
		if id == "" {
			continue
		}
		var mine []podObj
		for _, p := range pods.Items {
			if p.Metadata.Labels[labelTask] == j.Metadata.Labels[labelTask] {
				mine = append(mine, p)
			}
		}
		st := mapStatus(j, filterPods(mine, id), now, b.opts)
		st.TaskID = id
		out = append(out, st)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].TaskID < out[k].TaskID })
	return out, nil
}

// Preflight checks that the backend can list Jobs and Pods in the namespace
// and records whether it can manage NetworkPolicies. With Options.PreflightJob
// it also runs a small test Job (see preflightJob).
func (b *Backend) Preflight(ctx context.Context) error {
	if err := b.checkRoleAccess(ctx); err != nil {
		return err
	}
	for _, c := range []struct{ resource, path string }{
		{"jobs", b.jobsPath()},
		{"pods", b.podsPath()},
	} {
		if err := b.do(ctx, http.MethodGet, c.path+"?limit=1", "", nil, nil); err != nil {
			if isCode(err, http.StatusUnauthorized) || isCode(err, http.StatusForbidden) {
				return fmt.Errorf("k8stask: no permission to list %s in namespace %q: %w", c.resource, b.ns, err)
			}
			return fmt.Errorf("k8stask: list %s in namespace %q: %w", c.resource, b.ns, err)
		}
	}
	b.checkNetworkPolicyAccess(ctx)
	b.startEnforcementCheck()
	if b.opts.PreflightJob {
		return b.preflightJob(ctx)
	}
	return nil
}
