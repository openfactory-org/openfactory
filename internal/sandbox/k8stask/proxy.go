package k8stask

import (
	"math"
	"net/url"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

const (
	// proxySidecarName is the name of the sidecar container.
	proxySidecarName = egress.ProxySidecarName
)

// sidecarActive reports whether the Pod of the task gets a proxy sidecar.
func (b *Backend) sidecarActive(task core.Task) bool {
	return restricted(task) && b.opts.EgressProxySidecarImage != ""
}

// lockActive reports whether the Pod of the task gets the egress lock: an init
// container that refuses direct connections to ports 80 and 443 from any user
// but the proxy.
func (b *Backend) lockActive(task core.Task) bool {
	return b.sidecarActive(task) && egress.LockEnabled(b.opts.EgressLock)
}

// controlViaProxy reports whether the Pod reaches the control plane through
// its proxy sidecar: the lock is on, and the control plane is on port 80 or 443.
func (b *Backend) controlViaProxy(task core.Task) bool {
	return b.lockActive(task) && egress.LockControlViaProxy(task.Env[core.TaskEnvControlURL])
}

// proxyUser returns the user of the proxy sidecar. With the lock, it differs
// from the user of the task container.
func (b *Backend) proxyUser(task core.Task) int64 {
	taskUser, image := b.podUser(task)
	if image || taskUser <= 0 {
		taskUser = 0
	}
	if b.lockActive(task) {
		// The pod template can set the user too, for the Pod or for one
		// container. The proxy user must differ from each of them.
		return egress.ProxyUserAvoiding(append(templateUsers(b.template), taskUser)...)
	}
	if b.opts.RunAsImageUser || b.opts.RunAsUser <= 0 {
		return initUser
	}
	return b.opts.RunAsUser
}

// proxyEnv returns the proxy variables of a restricted task, or nil. The
// control plane and the loopback addresses bypass the proxy. A NetworkPolicy
// already limits the way to the control plane.
func (b *Backend) proxyEnv(task core.Task) map[string]string {
	if !b.sidecarActive(task) {
		return nil
	}
	host := ""
	if cu, err := url.Parse(task.Env[core.TaskEnvControlURL]); err == nil && !b.controlViaProxy(task) {
		host = cu.Hostname()
	}
	env := egress.Env(egress.SidecarURL(), host)
	// The runner reads the blocked hosts here and reports them.
	env[egress.ReportEnv] = egress.SidecarReportURL()
	return env
}

// proxySidecar returns the native sidecar that runs the netguard proxy for
// one task (package egress).
func (b *Backend) proxySidecar(task core.Task) map[string]any {
	var always []string
	if b.controlViaProxy(task) {
		if cu, err := url.Parse(task.Env[core.TaskEnvControlURL]); err == nil {
			always = append(always, cu.Hostname())
		}
	}
	return egress.Sidecar(egress.SidecarParams{
		Image: b.opts.EgressProxySidecarImage, PullPolicy: b.opts.ImagePullPolicy,
		User: b.proxyUser(task), Policy: task.NetworkPolicy, AlwaysAllow: always,
	})
}

// egressLock returns the init container of the egress lock.
func (b *Backend) egressLock(task core.Task) map[string]any {
	return egress.Lock(egress.LockParams{
		Image: b.opts.EgressProxySidecarImage, PullPolicy: b.opts.ImagePullPolicy,
		ProxyUser: b.proxyUser(task),
	})
}

// withProxySidecar puts the sidecar first in the init containers.
func withProxySidecar(init []any, sidecar map[string]any) []any {
	return egress.WithSidecar(init, sidecar)
}

// waitActive reports whether the Pod of the task gets the init container that
// waits until its NetworkPolicy is active. A network plugin applies the policy
// a few hundred milliseconds after the Pod starts. Without the wait, the task
// could open a connection in that time. The wait is off for a task that is
// not restricted, and when the policy cannot take effect: the operator allows
// an unenforced policy, or the check found that the cluster does not enforce
// NetworkPolicy.
func (b *Backend) waitActive(task core.Task) bool {
	switch {
	case !restricted(task), b.opts.EgressWaitTimeout < 0:
		return false
	case b.opts.AllowUnenforcedNetworkPolicy:
		return false
	case b.NetworkPolicyEnforcement() == egress.NotEnforced:
		return false
	}
	return true
}

// egressWait returns the init container of the wait. It runs `openfactory`
// from the first image that is set: the wait image, the runner image, the
// proxy sidecar image, or else the task image, which holds the runner then.
func (b *Backend) egressWait(task core.Task, runnerImage string) map[string]any {
	image := task.Image
	for _, c := range []string{b.opts.EgressProxySidecarImage, runnerImage, b.opts.EgressWaitImage} {
		if c != "" {
			image = c
		}
	}
	return egress.Wait(egress.WaitParams{
		Image: image, PullPolicy: b.opts.ImagePullPolicy,
		Timeout: b.opts.EgressWaitTimeout, Addr: b.opts.EgressWaitAddr,
	})
}

// templateUsers returns the runAsUser values of the pod template: the one of
// the Pod and the ones of its containers and init containers. The backend
// replaces the proxy sidecar and the lock, so their values do not count.
func templateUsers(tmpl map[string]any) []int64 {
	var out []int64
	add := func(sc any) {
		if u, ok := intValue(asMap(sc)["runAsUser"]); ok {
			out = append(out, u)
		}
	}
	add(tmpl["securityContext"])
	for _, key := range []string{"containers", "initContainers"} {
		list, _ := tmpl[key].([]any)
		for _, c := range list {
			m := asMap(c)
			if m == nil || m["name"] == proxySidecarName || m["name"] == egress.LockName {
				continue
			}
			add(m["securityContext"])
		}
	}
	return out
}

// intValue reads a number of a parsed template.
func intValue(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	case float64:
		return int64(n), true
	}
	return 0, false
}
