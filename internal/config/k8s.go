package config

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/sandbox/egress"
)

// RunAsImage is the value of OF_K8S_RUN_AS_USER that keeps the user of the image.
const RunAsImage = "image"

// DefaultK8sRunAsUser is the user of a task Pod when OF_K8S_RUN_AS_USER is empty.
const DefaultK8sRunAsUser int64 = 1000

var (
	labelKeyRe   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)
	labelValueRe = regexp.MustCompile(`^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$`)
	quantityRe   = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E|m)?$`)
)

// parseKeyValues parses "a=b,c=d". It checks the keys as Kubernetes label or
// annotation keys. With labels set, it checks the values as label values.
func parseKeyValues(env, v string, labels bool) (map[string]string, error) {
	out := map[string]string{}
	for _, term := range strings.Split(v, ",") {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		k, val, ok := strings.Cut(term, "=")
		k, val = strings.TrimSpace(k), strings.TrimSpace(val)
		if !ok || k == "" {
			return nil, fmt.Errorf("%s: %q must be key=value", env, term)
		}
		name := k[strings.LastIndex(k, "/")+1:]
		if len(k) > 253+1+63 || len(name) > 63 || !labelKeyRe.MatchString(k) {
			return nil, fmt.Errorf("%s: %q is not a valid key", env, k)
		}
		if labels && (len(val) > 63 || !labelValueRe.MatchString(val)) {
			return nil, fmt.Errorf("%s: %q is not a valid label value for key %q", env, val, k)
		}
		out[k] = val
	}
	return out, nil
}

// K8sLabelMap returns OF_K8S_LABELS. A bad value gives an empty map; Validate reports it.
func (c *Config) K8sLabelMap() map[string]string {
	m, _ := parseKeyValues("OF_K8S_LABELS", c.K8sLabels, true)
	return m
}

// K8sAnnotationMap returns OF_K8S_ANNOTATIONS.
func (c *Config) K8sAnnotationMap() map[string]string {
	m, _ := parseKeyValues("OF_K8S_ANNOTATIONS", c.K8sAnnotations, false)
	return m
}

// K8sImagePullPolicyValue returns OF_K8S_IMAGE_PULL_POLICY. Empty means IfNotPresent.
func (c *Config) K8sImagePullPolicyValue() string {
	if v := strings.TrimSpace(c.K8sImagePullPolicy); v != "" {
		return v
	}
	return "IfNotPresent"
}

// K8sRunAsUserValue returns the user of a task Pod. With image set, the Pod
// keeps the user of the image and the number is 0.
func (c *Config) K8sRunAsUserValue() (user int64, image bool) {
	v := strings.TrimSpace(c.K8sRunAsUser)
	switch {
	case v == "":
		return DefaultK8sRunAsUser, false
	case strings.EqualFold(v, RunAsImage):
		return 0, true
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 1 {
		return DefaultK8sRunAsUser, false
	}
	return n, false
}

// validateK8s checks the settings of the Kubernetes task Jobs.
func (c *Config) validateK8s() []error {
	var errs []error
	if _, err := parseKeyValues("OF_K8S_LABELS", c.K8sLabels, true); err != nil {
		errs = append(errs, err)
	}
	if _, err := parseKeyValues("OF_K8S_ANNOTATIONS", c.K8sAnnotations, false); err != nil {
		errs = append(errs, err)
	}
	switch c.K8sImagePullPolicyValue() {
	case "Always", "IfNotPresent", "Never":
	default:
		errs = append(errs, fmt.Errorf("OF_K8S_IMAGE_PULL_POLICY %q: want Always, IfNotPresent or Never", c.K8sImagePullPolicy))
	}
	if v := strings.TrimSpace(c.K8sRunAsUser); v != "" && !strings.EqualFold(v, RunAsImage) {
		if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("OF_K8S_RUN_AS_USER %q: want a number of 1 or more, or %q", c.K8sRunAsUser, RunAsImage))
		}
	}
	if v := strings.TrimSpace(c.K8sWorkspaceSizeLimit); v != "" && !quantityRe.MatchString(v) {
		errs = append(errs, fmt.Errorf("OF_K8S_WORKSPACE_SIZE_LIMIT %q: want a quantity such as 20Gi", c.K8sWorkspaceSizeLimit))
	}
	if strings.TrimSpace(c.K8sControlPlaneSelector) == "" {
		if strings.TrimSpace(c.K8sControlPlaneNamespace) != "" {
			errs = append(errs, fmt.Errorf("OF_K8S_CONTROL_PLANE_NAMESPACE is set without OF_K8S_CONTROL_PLANE_SELECTOR"))
		}
	} else if _, err := parseKeyValues("OF_K8S_CONTROL_PLANE_SELECTOR", c.K8sControlPlaneSelector, true); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, c.validateNetworkPolicy()...)
	return errs
}

// K8sNetworkPolicyCheckValue returns OF_K8S_NETWORK_POLICY_CHECK. The default is "probe".
func (c *Config) K8sNetworkPolicyCheckValue() string {
	if v := strings.ToLower(strings.TrimSpace(c.K8sNetworkPolicyCheck)); v != "" {
		return v
	}
	return "probe"
}

// K8sNetworkPolicyEnforcementValue returns OF_K8S_NETWORK_POLICY_ENFORCEMENT. The default is "warn".
func (c *Config) K8sNetworkPolicyEnforcementValue() string {
	if v := strings.ToLower(strings.TrimSpace(c.K8sNetworkPolicyEnforcement)); v != "" {
		return v
	}
	return "warn"
}

// K8sEgressLockValue returns OF_K8S_EGRESS_LOCK. The default is "none".
func (c *Config) K8sEgressLockValue() string {
	if v := strings.ToLower(strings.TrimSpace(c.K8sEgressLock)); v != "" {
		return v
	}
	return "none"
}

// EgressWaitOff is the value of OF_K8S_EGRESS_WAIT_TIMEOUT that turns the wait off.
const EgressWaitOff = "off"

// minEgressWaitTimeout is the shortest time that the wait can have. A shorter
// time would end before a normal network plugin applies the policy.
const minEgressWaitTimeout = time.Second

// K8sEgressWaitTimeoutValue returns OF_K8S_EGRESS_WAIT_TIMEOUT for the
// backend: 0 means the default (30 seconds), and a negative value means that
// the wait is off. A bad value gives 0; Validate reports it.
func (c *Config) K8sEgressWaitTimeoutValue() time.Duration {
	d, _ := parseEgressWaitTimeout(c.K8sEgressWaitTimeout)
	return d
}

func parseEgressWaitTimeout(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "":
		return 0, nil
	case strings.EqualFold(v, EgressWaitOff):
		return -1, nil
	}
	d, err := time.ParseDuration(v)
	switch {
	case err != nil:
		return 0, fmt.Errorf("OF_K8S_EGRESS_WAIT_TIMEOUT %q: want a duration such as 60s, or %q", v, EgressWaitOff)
	case d < minEgressWaitTimeout:
		return 0, fmt.Errorf("OF_K8S_EGRESS_WAIT_TIMEOUT %q: must be at least %s; use %q to turn the wait off", v, minEgressWaitTimeout, EgressWaitOff)
	}
	return d, nil
}

// validateNetworkPolicy checks the settings of the network policy check, the
// egress lock and the wait for the policy.
func (c *Config) validateNetworkPolicy() []error {
	var errs []error
	switch c.K8sNetworkPolicyCheckValue() {
	case "probe", "off":
	default:
		errs = append(errs, fmt.Errorf("OF_K8S_NETWORK_POLICY_CHECK %q: want probe or off", c.K8sNetworkPolicyCheck))
	}
	switch c.K8sNetworkPolicyEnforcementValue() {
	case "warn":
	case "require":
		if c.K8sNetworkPolicyCheckValue() == "off" {
			errs = append(errs, fmt.Errorf("OF_K8S_NETWORK_POLICY_ENFORCEMENT=require needs OF_K8S_NETWORK_POLICY_CHECK=probe"))
		}
	default:
		errs = append(errs, fmt.Errorf("OF_K8S_NETWORK_POLICY_ENFORCEMENT %q: want warn or require", c.K8sNetworkPolicyEnforcement))
	}
	switch c.K8sEgressLockValue() {
	case "none":
	case "iptables":
		if strings.TrimSpace(c.K8sEgressProxySidecarImage) == "" {
			errs = append(errs, fmt.Errorf("OF_K8S_EGRESS_LOCK=iptables needs OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE"))
		}
	default:
		errs = append(errs, fmt.Errorf("OF_K8S_EGRESS_LOCK %q: want none or iptables", c.K8sEgressLock))
	}
	if _, err := parseEgressWaitTimeout(c.K8sEgressWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if err := egress.ValidateWaitAddr(strings.TrimSpace(c.K8sEgressWaitAddr)); err != nil {
		errs = append(errs, fmt.Errorf("OF_K8S_EGRESS_WAIT_ADDR: %w", err))
	}
	return errs
}
