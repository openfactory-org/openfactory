package k8stask

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

const (
	netpolOwnerAPI  = "batch/v1"
	netpolOwnerKind = "Job"
	// cleanupTimeout is the time for the delete calls after a failed start.
	cleanupTimeout = 30 * time.Second
)

// restricted reports whether the task needs a NetworkPolicy.
func restricted(task core.Task) bool { return egress.Restricted(task.NetworkPolicy) }

// peer is a set of Pods that a restricted task can reach.
type peer = egress.Peer

// newPeer parses a label selector string of "key=value" terms.
func newPeer(what, namespace, selector string) (peer, error) {
	return egress.NewPeer(what, namespace, selector)
}

func (b *Backend) netpolsPath() string {
	return "/apis/networking.k8s.io/v1/namespaces/" + url.PathEscape(b.ns) + "/networkpolicies"
}

// controlURLRule returns an egress rule for the host of the control URL if
// that host is an IP address, else nil.
func controlURLRule(raw string) map[string]any { return egress.ControlURLRule(raw) }

// networkPolicyManifest builds the NetworkPolicy of a restricted task. It
// selects the Pod by its task label and allows egress to DNS, to the control
// plane and, with a proxy sidecar, to the proxy ports only (package egress).
func (b *Backend) networkPolicyManifest(task core.Task) map[string]any {
	return egress.Manifest(egress.PolicyParams{
		Name: jobName(task.TaskID), Namespace: b.ns,
		SelectorKey: labelTask, SelectorValue: taskLabel(task.TaskID),
		Labels: map[string]any{
			labelTask:                      taskLabel(task.TaskID),
			"app.kubernetes.io/managed-by": "openfactory",
		},
		Annotations:  map[string]any{annTaskID: task.TaskID},
		ControlPlane: b.cp,
		ControlURL:   task.Env[core.TaskEnvControlURL],
		Sidecar:      b.sidecarActive(task),
	})
}

// ensureNetworkPolicy creates the NetworkPolicy of a restricted task. It
// returns true if it created a new one. A policy that exists is kept. If the
// create fails, Start fails with an error that wraps core.ErrInvalid, unless
// Options.AllowUnenforcedNetworkPolicy is set. Then the backend logs a warning.
func (b *Backend) ensureNetworkPolicy(ctx context.Context, task core.Task) (bool, error) {
	if err := egress.Gate(ctx, b.enforce, b.opts.NetworkPolicyEnforcement,
		b.opts.AllowUnenforcedNetworkPolicy, b.log, task.TaskID); err != nil {
		return false, fmt.Errorf("k8stask: %w", err)
	}
	err := b.do(ctx, http.MethodPost, b.netpolsPath(), "", b.networkPolicyManifest(task), nil)
	switch {
	case err == nil:
		return true, nil
	case isCode(err, http.StatusConflict):
		return false, nil
	}
	if b.opts.AllowUnenforcedNetworkPolicy {
		b.log.Warn("network policy is NOT enforced: the NetworkPolicy of the task could not be created"+
			" and OF_ALLOW_UNENFORCED_NETWORK_POLICY is set", "task", task.TaskID, "err", err)
		return false, nil
	}
	hint := ""
	if b.netpol.Load() == netpolDenied {
		hint = " (the preflight found that the Role has no permission for networkpolicies)"
	}
	return false, fmt.Errorf("k8stask: create network policy for restricted task%s: %w: %w: "+
		"set OF_ALLOW_UNENFORCED_NETWORK_POLICY=true only if another control enforces egress",
		hint, core.ErrInvalid, err)
}

// ownNetworkPolicy makes the Job the owner of the NetworkPolicy, so that the
// NetworkPolicy goes with the Job.
func (b *Backend) ownNetworkPolicy(ctx context.Context, taskID, jobUID string) error {
	if jobUID == "" {
		return fmt.Errorf("k8stask: the API did not return a UID for the Job of task %q", taskID)
	}
	patch := map[string]any{"metadata": map[string]any{"ownerReferences": []any{map[string]any{
		"apiVersion": netpolOwnerAPI, "kind": netpolOwnerKind,
		"name": jobName(taskID), "uid": jobUID,
	}}}}
	err := b.do(ctx, http.MethodPatch, b.netpolsPath()+"/"+url.PathEscape(jobName(taskID)),
		"application/merge-patch+json", patch, nil)
	if err != nil {
		return fmt.Errorf("k8stask: set the owner of the network policy: %w", err)
	}
	return nil
}

// discard deletes the Job and the NetworkPolicy of a task whose Start failed.
// It uses a fresh context, because the context of Start can be done.
func (b *Backend) discard(taskID string) {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	body := map[string]any{"kind": "DeleteOptions", "apiVersion": "v1", "propagationPolicy": "Background"}
	err := b.do(ctx, http.MethodDelete, b.jobsPath()+"/"+url.PathEscape(jobName(taskID)), "", body, nil)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		b.log.Warn("delete job after a failed start", "task", taskID, "err", err)
	}
	b.deleteNetworkPolicy(ctx, taskID)
}

func (b *Backend) deleteNetworkPolicy(ctx context.Context, taskID string) {
	if err := b.deleteNetworkPolicyErr(ctx, taskID); err != nil {
		b.log.Warn("delete network policy", "task", taskID, "err", err)
	}
}

// deleteNetworkPolicyErr deletes the NetworkPolicy of a task. A missing
// policy is not an error. If Preflight found no permission, a 403 is not an
// error either: the backend never created a policy.
func (b *Backend) deleteNetworkPolicyErr(ctx context.Context, taskID string) error {
	err := b.do(ctx, http.MethodDelete, b.netpolsPath()+"/"+url.PathEscape(jobName(taskID)), "", nil, nil)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		if isCode(err, http.StatusForbidden) && b.netpol.Load() == netpolDenied {
			return nil
		}
		return fmt.Errorf("k8stask: delete network policy: %w", err)
	}
	return nil
}

// checkNetworkPolicyAccess asks the API whether the backend can create, get, patch
// and delete NetworkPolicies in the namespace, and records the answer. If the
// check fails, the answer stays unknown.
func (b *Backend) checkNetworkPolicyAccess(ctx context.Context) {
	denied, err := egress.CheckAccess(ctx, b.ns, func(ctx context.Context, body map[string]any, out any) error {
		return b.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", "", body, out)
	})
	if err != nil {
		b.log.Debug("preflight: access review failed", "err", err)
		return
	}
	if len(denied) == 0 {
		b.netpol.Store(netpolAllowed)
		return
	}
	b.netpol.Store(netpolDenied)
	log := b.log.Error
	if b.opts.AllowUnenforcedNetworkPolicy {
		log = b.log.Warn
	}
	log("the backend cannot manage NetworkPolicies in the sandbox namespace: restricted tasks cannot start",
		"namespace", b.ns, "denied_verbs", strings.Join(denied, ","),
		"fix", "add networkpolicies (create, get, patch, delete) to the Role, or set OF_ALLOW_UNENFORCED_NETWORK_POLICY=true")
}

// NetworkPolicyAccess reports what Preflight found. known is false if
// Preflight did not run or could not tell. Else ok says whether the backend
// can manage NetworkPolicies.
func (b *Backend) NetworkPolicyAccess() (known, ok bool) {
	switch b.netpol.Load() {
	case netpolAllowed:
		return true, true
	case netpolDenied:
		return true, false
	}
	return false, false
}

// initEnforcementCheck makes the checker of the cluster, if the setting asks
// for it and an image is known.
func (b *Backend) initEnforcementCheck() {
	if b.opts.NetworkPolicyCheck != egress.CheckProbe {
		return
	}
	image := b.opts.NetworkPolicyProbeImage
	for _, c := range []string{b.opts.EgressProxySidecarImage, b.opts.RunnerImage} {
		if image == "" {
			image = c
		}
	}
	if image == "" {
		log := b.log.Info
		if b.opts.NetworkPolicyEnforcement == egress.EnforcementRequire {
			log = b.log.Warn
		}
		log("the NetworkPolicy check has no image: set OF_RUNNER_IMAGE or OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE")
		return
	}
	b.enforce = egress.NewChecker(func(ctx context.Context) (egress.Enforcement, error) {
		return egress.Probe(ctx, egress.ProbeParams{
			Do: func(ctx context.Context, method, path string, body, out any) error {
				return b.do(ctx, method, path, "", body, out)
			},
			Namespace: b.ns, Image: image, PullPolicy: b.opts.ImagePullPolicy,
			ImagePullSecrets: b.opts.ImagePullSecrets, Template: b.template, Logger: b.log,
		})
	}, b.log)
}

// startEnforcementCheck runs the check in the background. It does not run if
// Preflight found that the Role cannot manage NetworkPolicies: the probe needs
// the same permissions.
func (b *Backend) startEnforcementCheck() {
	if b.enforce == nil || b.netpol.Load() == netpolDenied {
		return
	}
	b.enforce.Start()
}

// NetworkPolicyEnforcement returns what the check found so far.
func (b *Backend) NetworkPolicyEnforcement() egress.Enforcement {
	if b.enforce == nil {
		return egress.EnforcementUnknown
	}
	return b.enforce.Cached()
}
