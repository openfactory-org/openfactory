package egress

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// A NetworkPolicy is only a request to the cluster. A cluster whose network
// plugin does not enforce it accepts the object and does nothing. The probe
// finds this out with a test. It needs no access outside the sandbox
// namespace and no knowledge of the plugin:
//
//  1. A target Pod listens on a TCP port.
//  2. A NetworkPolicy denies all egress for the Pods with the role "denied".
//  3. Two client Pods try to connect to the target. The "control" Pod has no
//     policy and must connect. The "denied" Pod has the deny policy.
//
// If the denied Pod connects, the cluster does not enforce NetworkPolicy. If it
// fails and the control Pod connected, the cluster enforces it. If the control
// Pod fails too, the test says nothing (for example, the image does not run).

// Enforcement is the result of the probe.
type Enforcement string

const (
	// EnforcementUnknown means that the check did not run or had no answer.
	EnforcementUnknown Enforcement = "unknown"
	// Enforced means that the cluster denied traffic that a policy denies.
	Enforced Enforcement = "enforced"
	// NotEnforced means that traffic passed although a policy denied it.
	NotEnforced Enforcement = "not_enforced"
)

// The settings of the check.
const (
	// CheckOff does not check. CheckProbe runs the probe in the background at
	// startup.
	CheckOff   = "off"
	CheckProbe = "probe"
	// EnforcementWarn logs a warning for a restricted task when the cluster does
	// not enforce policies. EnforcementRequire fails the task.
	EnforcementWarn    = "warn"
	EnforcementRequire = "require"
)

// ValidateCheck checks the value of the check setting. An empty value is valid.
func ValidateCheck(v string) error {
	switch v {
	case "", CheckOff, CheckProbe:
		return nil
	}
	return fmt.Errorf("network policy check %q: want %q or %q", v, CheckOff, CheckProbe)
}

// ValidateEnforcement checks the value of the enforcement setting. An empty
// value is valid.
func ValidateEnforcement(v string) error {
	switch v {
	case "", EnforcementWarn, EnforcementRequire:
		return nil
	}
	return fmt.Errorf("network policy enforcement %q: want %q or %q", v, EnforcementWarn, EnforcementRequire)
}

const (
	// LabelProbe marks the objects of a probe. Its value is the probe ID.
	LabelProbe     = "openfactory.dev/netprobe"
	labelProbeRole = "openfactory.dev/netprobe-role"
	// ProbePort is the port of the target Pod.
	ProbePort = 8080

	roleTarget  = "target"
	roleControl = "control"
	roleDenied  = "denied"
	// roleRecheck is a second denied client. It has the label of the denied
	// role, so the deny policy selects it too.
	roleRecheck = "recheck"

	probeUser        = 65532
	probeDeadline    = 300 // activeDeadlineSeconds: a leaked Pod ends by itself
	defaultTimeout   = 3 * time.Minute
	defaultPullGrace = 45 * time.Second
	defaultPoll      = time.Second
)

// Do calls the Kubernetes API. A body that is not nil is sent as JSON, and a
// response that out points to is decoded. A 404 gives an error that wraps
// core.ErrNotFound. Both backends have such a function.
type Do func(ctx context.Context, method, path string, body, out any) error

// ProbeParams describes one probe.
type ProbeParams struct {
	Do        Do
	Namespace string
	// Image holds the openfactory binary. The probe runs
	// `openfactory netprobe` from it.
	Image, PullPolicy string
	ImagePullSecrets  []string
	// Template is the PodSpec template of the operator. The probe copies
	// the fields that place a Pod (node selector, tolerations, affinity, runtime
	// class, priority class).
	Template map[string]any
	// Timeout is the time for the whole probe. Default 3 minutes.
	Timeout time.Duration
	// PullGrace is the time after which a pull error ends the probe. Default 45 seconds.
	PullGrace time.Duration
	// Poll is the time between two reads of a Pod. Default 1 second.
	Poll time.Duration
	// DeniedDelay is the time that the denied client waits before it
	// connects, so that the network plugin can apply the policy. Default 3 seconds.
	DeniedDelay time.Duration
	// RecheckDelay is the wait of a second denied client, which runs only when
	// the first one connected. A busy network plugin can need more than
	// DeniedDelay to apply a new policy, and a false "not enforced" is a wrong
	// error for the operator. Default 15 seconds.
	RecheckDelay time.Duration
	Logger       *slog.Logger
}

func (p *ProbeParams) defaults() {
	if p.Timeout <= 0 {
		p.Timeout = defaultTimeout
	}
	if p.PullGrace <= 0 {
		p.PullGrace = defaultPullGrace
	}
	if p.Poll <= 0 {
		p.Poll = defaultPoll
	}
	if p.DeniedDelay <= 0 {
		p.DeniedDelay = 3 * time.Second
	}
	if p.RecheckDelay <= 0 {
		p.RecheckDelay = 15 * time.Second
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
}

// Probe runs the test and removes its objects. It returns Enforced or
// NotEnforced, or EnforcementUnknown with an error that says why the test
// had no answer.
func Probe(ctx context.Context, p ProbeParams) (Enforcement, error) {
	p.defaults()
	if p.Do == nil || p.Image == "" {
		return EnforcementUnknown, errors.New("the probe needs an API client and an image")
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()

	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	id := hex.EncodeToString(rnd[:])
	base := "of-netprobe-" + id
	pr := &prober{ProbeParams: p, id: id, base: base}

	defer func() {
		// The context of the probe can be done, so the cleanup has its own.
		cctx, c := context.WithTimeout(context.Background(), 30*time.Second)
		defer c()
		pr.cleanup(cctx)
	}()

	ip, err := pr.startTarget(ctx)
	if err != nil {
		return EnforcementUnknown, err
	}
	if err := pr.create(ctx, pr.netpolPath(), pr.denyPolicy()); err != nil {
		return EnforcementUnknown, fmt.Errorf("create the deny policy: %w", err)
	}
	addr := ip + ":" + strconv.Itoa(ProbePort)
	if err := pr.createPod(ctx, roleControl, 0, addr); err != nil {
		return EnforcementUnknown, err
	}
	if err := pr.createPod(ctx, roleDenied, p.DeniedDelay, addr); err != nil {
		return EnforcementUnknown, err
	}
	control, err := pr.waitDone(ctx, roleControl)
	if err != nil {
		return EnforcementUnknown, err
	}
	denied, err := pr.waitDone(ctx, roleDenied)
	if err != nil {
		return EnforcementUnknown, err
	}
	switch {
	case !control:
		return EnforcementUnknown, errors.New("the control Pod could not connect to the target Pod, " +
			"so the test has no answer (check that the image can run and that the cluster allows Pod to Pod traffic)")
	case denied:
		// Check again with more time for the plugin before the answer.
		pr.Logger.Info("network policy probe: the denied client connected; checking again", "wait", p.RecheckDelay)
		if err := pr.createPod(ctx, roleRecheck, p.RecheckDelay, addr); err != nil {
			return EnforcementUnknown, err
		}
		again, err := pr.waitDone(ctx, roleRecheck)
		if err != nil {
			return EnforcementUnknown, err
		}
		if again {
			return NotEnforced, nil
		}
	}
	return Enforced, nil
}

type prober struct {
	ProbeParams
	id, base string

	mu             sync.Mutex
	firstPullError map[string]time.Time
}

func (pr *prober) podName(role string) string { return pr.base + "-" + role }
func (pr *prober) podsPath() string {
	return "/api/v1/namespaces/" + url.PathEscape(pr.Namespace) + "/pods"
}
func (pr *prober) netpolPath() string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(pr.Namespace) + "/networkpolicies"
}

func (pr *prober) labels(role string) map[string]any {
	return map[string]any{
		LabelProbe:                     pr.id,
		labelProbeRole:                 labelRole(role),
		"app.kubernetes.io/managed-by": "openfactory",
	}
}

// labelRole is the role label of a Pod: the recheck client is a denied client.
func labelRole(role string) string {
	if role == roleRecheck {
		return roleDenied
	}
	return role
}

// denyPolicy denies all egress for the denied client.
func (pr *prober) denyPolicy() map[string]any {
	return map[string]any{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": map[string]any{
			"name": pr.base, "namespace": pr.Namespace, "labels": pr.labels("policy"),
		},
		"spec": map[string]any{
			"podSelector": map[string]any{"matchLabels": map[string]any{labelProbeRole: roleDenied, LabelProbe: pr.id}},
			"policyTypes": []any{"Egress"},
			"egress":      []any{},
		},
	}
}

func (pr *prober) create(ctx context.Context, path string, obj map[string]any) error {
	return pr.Do(ctx, http.MethodPost, path, obj, nil)
}

func (pr *prober) spec(command []any, ready bool) map[string]any {
	c := map[string]any{
		"name":            "probe",
		"image":           pr.Image,
		"imagePullPolicy": pr.PullPolicy,
		"command":         command,
		"securityContext": map[string]any{
			"runAsNonRoot":             true,
			"runAsUser":                probeUser,
			"allowPrivilegeEscalation": false,
			"readOnlyRootFilesystem":   true,
			"capabilities":             map[string]any{"drop": []any{"ALL"}},
			"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
		},
		"resources": map[string]any{
			"requests": map[string]any{"cpu": "10m", "memory": "16Mi"},
			"limits":   map[string]any{"memory": "64Mi"},
		},
	}
	if ready {
		c["readinessProbe"] = map[string]any{
			"tcpSocket":     map[string]any{"port": ProbePort},
			"periodSeconds": 1,
		}
	}
	spec := map[string]any{
		"restartPolicy":                 "Never",
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"terminationGracePeriodSeconds": 0,
		"activeDeadlineSeconds":         probeDeadline,
		"containers":                    []any{c},
		"nodeSelector":                  map[string]any{"kubernetes.io/os": "linux"},
	}
	for _, k := range []string{"tolerations", "affinity", "runtimeClassName", "priorityClassName"} {
		if v, ok := pr.Template[k]; ok {
			spec[k] = v
		}
	}
	if ns, ok := pr.Template["nodeSelector"].(map[string]any); ok {
		merged := map[string]any{"kubernetes.io/os": "linux"}
		for k, v := range ns {
			merged[k] = v
		}
		spec["nodeSelector"] = merged
	}
	var secrets []any
	for _, n := range pr.ImagePullSecrets {
		secrets = append(secrets, map[string]any{"name": n})
	}
	if len(secrets) > 0 {
		spec["imagePullSecrets"] = secrets
	} else if v, ok := pr.Template["imagePullSecrets"]; ok {
		spec["imagePullSecrets"] = v
	}
	return spec
}

func (pr *prober) pod(role string, command []any, ready bool) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name": pr.podName(role), "namespace": pr.Namespace, "labels": pr.labels(role),
		},
		"spec": pr.spec(command, ready),
	}
}

func (pr *prober) startTarget(ctx context.Context) (string, error) {
	cmd := []any{"openfactory", "netprobe", "serve", "--listen", ":" + strconv.Itoa(ProbePort)}
	if err := pr.create(ctx, pr.podsPath(), pr.pod(roleTarget, cmd, true)); err != nil {
		return "", fmt.Errorf("create the target Pod: %w", err)
	}
	for {
		st, err := pr.status(ctx, roleTarget)
		if err != nil {
			return "", err
		}
		if err := pr.failed(st, roleTarget); err != nil {
			return "", err
		}
		if st.ready && st.podIP != "" {
			return st.podIP, nil
		}
		if err := pr.wait(ctx, "the target Pod did not become ready", st); err != nil {
			return "", err
		}
	}
}

func (pr *prober) createPod(ctx context.Context, role string, delay time.Duration, addr string) error {
	cmd := []any{"openfactory", "netprobe", "dial", "--addr", addr,
		"--delay", delay.String(), "--attempts", "3", "--interval", "2s", "--timeout", "2s"}
	if role == roleControl {
		cmd = []any{"openfactory", "netprobe", "dial", "--addr", addr,
			"--attempts", "15", "--interval", "1s", "--timeout", "2s"}
	}
	if err := pr.create(ctx, pr.podsPath(), pr.pod(role, cmd, false)); err != nil {
		return fmt.Errorf("create the %s Pod: %w", role, err)
	}
	return nil
}

// waitDone waits until the client Pod ends. It returns true if the client
// connected (exit code 0).
func (pr *prober) waitDone(ctx context.Context, role string) (bool, error) {
	for {
		st, err := pr.status(ctx, role)
		if err != nil {
			return false, err
		}
		if err := pr.failed(st, role); err != nil {
			return false, err
		}
		switch st.phase {
		case "Succeeded":
			return true, nil
		case "Failed":
			if st.exitCode == 1 {
				return false, nil
			}
			return false, fmt.Errorf("the %s Pod failed (exit code %d): %s", role, st.exitCode, st.message)
		}
		if err := pr.wait(ctx, "the "+role+" Pod did not end", st); err != nil {
			return false, err
		}
	}
}

func (pr *prober) wait(ctx context.Context, what string, st podState) error {
	select {
	case <-ctx.Done():
		return fmt.Errorf("%s in time (last phase %q): %w", what, st.phase, ctx.Err())
	case <-time.After(pr.Poll):
		return nil
	}
}

type podState struct {
	phase, podIP, message string
	ready                 bool
	exitCode              int
	waitingReason         string
	waitingMessage        string
}

type podJSON struct {
	Metadata struct {
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		PodIP      string `json:"podIP"`
		Message    string `json:"message"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			State struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Message  string `json:"message"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (pr *prober) status(ctx context.Context, role string) (podState, error) {
	var p podJSON
	err := pr.Do(ctx, http.MethodGet, pr.podsPath()+"/"+url.PathEscape(pr.podName(role)), nil, &p)
	if err != nil {
		return podState{}, fmt.Errorf("read the %s Pod: %w", role, err)
	}
	st := podState{phase: p.Status.Phase, podIP: p.Status.PodIP, message: p.Status.Message}
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			st.ready = true
		}
	}
	if len(p.Status.ContainerStatuses) > 0 {
		cs := p.Status.ContainerStatuses[0].State
		if cs.Terminated != nil {
			st.exitCode = cs.Terminated.ExitCode
			if cs.Terminated.Message != "" {
				st.message = cs.Terminated.Message
			}
		}
		if cs.Waiting != nil {
			st.waitingReason, st.waitingMessage = cs.Waiting.Reason, cs.Waiting.Message
		}
	}
	return st, nil
}

// failed returns an error if the Pod cannot run: the image does not pull.
func (pr *prober) failed(st podState, role string) error {
	switch st.waitingReason {
	case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError":
		if pr.pullWaited(role) {
			return fmt.Errorf("the %s Pod cannot start (%s): %s: check the image %q, the registry and the pull secrets",
				role, st.waitingReason, st.waitingMessage, pr.Image)
		}
	}
	return nil
}

// pullWaited keeps the time of the first pull error of a role, and reports
// whether the grace time has passed.
func (pr *prober) pullWaited(role string) bool {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.firstPullError == nil {
		pr.firstPullError = map[string]time.Time{}
	}
	t, ok := pr.firstPullError[role]
	if !ok {
		pr.firstPullError[role] = time.Now()
		return pr.PullGrace <= 0
	}
	return time.Since(t) >= pr.PullGrace
}

func (pr *prober) cleanup(ctx context.Context) {
	body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "gracePeriodSeconds": 0}
	for _, role := range []string{roleTarget, roleControl, roleDenied, roleRecheck} {
		path := pr.podsPath() + "/" + url.PathEscape(pr.podName(role))
		if err := pr.Do(ctx, http.MethodDelete, path, body, nil); err != nil && !errors.Is(err, core.ErrNotFound) {
			pr.Logger.Warn("network policy probe: delete Pod", "pod", pr.podName(role), "err", err)
		}
	}
	err := pr.Do(ctx, http.MethodDelete, pr.netpolPath()+"/"+url.PathEscape(pr.base), nil, nil)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		pr.Logger.Warn("network policy probe: delete NetworkPolicy", "policy", pr.base, "err", err)
	}
}

// Checker runs a check once and keeps the answer. A conclusive answer is kept
// for the life of the process. An answer without a result is tried again after
// RetryAfter.
type Checker struct {
	run func(ctx context.Context) (Enforcement, error)
	log *slog.Logger
	// RetryAfter is the time before a check that had no answer runs again.
	// Default 5 minutes.
	RetryAfter time.Duration

	mu       sync.Mutex
	state    Enforcement
	err      error
	finished time.Time
	inflight chan struct{}
}

// NewChecker returns a checker that calls run when it needs an answer.
func NewChecker(run func(ctx context.Context) (Enforcement, error), log *slog.Logger) *Checker {
	if log == nil {
		log = slog.Default()
	}
	return &Checker{run: run, log: log, state: EnforcementUnknown}
}

// Cached returns the answer so far, without waiting.
func (c *Checker) Cached() Enforcement {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// Start runs the check in the background if it has not run. It logs the
// answer: an error if the cluster does not enforce policies.
func (c *Checker) Start() {
	go func() {
		_, _ = c.Result(context.Background())
	}()
}

// Result returns the answer. If a check runs, it waits for it, until ctx ends.
// If no answer exists, it starts one.
func (c *Checker) Result(ctx context.Context) (Enforcement, error) {
	c.mu.Lock()
	if c.state != EnforcementUnknown {
		st := c.state
		c.mu.Unlock()
		return st, nil
	}
	retry := c.RetryAfter
	if retry <= 0 {
		retry = 5 * time.Minute
	}
	if c.inflight == nil && c.err != nil && time.Since(c.finished) < retry {
		err := c.err
		c.mu.Unlock()
		return EnforcementUnknown, err
	}
	done := c.inflight
	if done == nil {
		done = make(chan struct{})
		c.inflight = done
		go c.execute(done) //nolint:gosec // shared check: a caller that gives up must not stop it (see execute)
	}
	c.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return EnforcementUnknown, ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.err
}

func (c *Checker) execute(done chan struct{}) {
	// The check does not depend on the context of the caller: a caller that
	// gives up must not stop a check that others wait for.
	st, err := c.run(context.Background())
	c.mu.Lock()
	c.state, c.err, c.finished = st, err, time.Now()
	c.inflight = nil
	c.mu.Unlock()
	close(done)
	switch {
	case err != nil:
		c.log.Warn("could not check whether the cluster enforces NetworkPolicy", "err", err)
	case st == NotEnforced:
		c.log.Error("the cluster does NOT enforce NetworkPolicy: the network plugin accepts the objects and ignores them. " +
			"Restricted runs have no network limit. Install a network plugin that enforces NetworkPolicy, " +
			"or set OF_K8S_NETWORK_POLICY_ENFORCEMENT=require to refuse restricted runs")
	case st == Enforced:
		c.log.Info("the cluster enforces NetworkPolicy (checked with a probe)")
	}
}

// Gate decides whether a restricted task may start. With the mode "require",
// the cluster must be known to enforce NetworkPolicy. A cluster that does
// not, or that could not be checked, fails the task with an error that wraps
// core.ErrInvalid, unless allowUnenforced is set. With the mode "warn" (the
// default), a task always starts, and a cluster that is known not to enforce
// policies gives a warning. A nil checker means that the check is off.
func Gate(ctx context.Context, c *Checker, mode string, allowUnenforced bool, log *slog.Logger, task string) error {
	if log == nil {
		log = slog.Default()
	}
	if mode != EnforcementRequire {
		if c != nil && c.Cached() == NotEnforced {
			log.Warn("network policy is NOT enforced by the cluster network plugin: this restricted task has no network limit",
				"task", task)
		}
		return nil
	}
	var (
		st  = EnforcementUnknown
		err error
	)
	if c != nil {
		st, err = c.Result(ctx)
	} else {
		err = errors.New("the check is off: set OF_K8S_NETWORK_POLICY_CHECK=probe")
	}
	if st == Enforced {
		return nil
	}
	if allowUnenforced {
		log.Warn("network policy enforcement is not confirmed, and OF_ALLOW_UNENFORCED_NETWORK_POLICY is set: the task starts",
			"task", task, "state", string(st))
		return nil
	}
	if st == NotEnforced {
		return fmt.Errorf("%w: the cluster does not enforce NetworkPolicy (found by a probe), so the restricted network policy "+
			"would not limit the run: install a network plugin that enforces NetworkPolicy, "+
			"or set OF_ALLOW_UNENFORCED_NETWORK_POLICY=true only if another control enforces egress", core.ErrInvalid)
	}
	return fmt.Errorf("%w: it is not known that the cluster enforces NetworkPolicy (%v): "+
		"fix the check, or set OF_K8S_NETWORK_POLICY_ENFORCEMENT=warn, "+
		"or set OF_ALLOW_UNENFORCED_NETWORK_POLICY=true only if another control enforces egress", core.ErrInvalid, err)
}
