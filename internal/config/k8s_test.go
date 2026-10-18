package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func loadK8s(t *testing.T, m map[string]string) (*Config, error) {
	t.Helper()
	m["OF_DEV"], m["OF_DATA_DIR"] = "1", t.TempDir()
	return LoadFrom(nil, env(m))
}

func TestK8sDefaults(t *testing.T) {
	c, err := loadK8s(t, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	user, image := c.K8sRunAsUserValue()
	if user != 1000 || image || c.K8sImagePullPolicyValue() != "IfNotPresent" ||
		len(c.K8sLabelMap()) != 0 || len(c.K8sAnnotationMap()) != 0 || c.K8sWorkspaceSizeLimit != "" ||
		c.K8sPreflightJob || c.AllowUnenforcedNetworkPolicy {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestK8sSettings(t *testing.T) {
	c, err := loadK8s(t, map[string]string{
		"OF_K8S_LABELS":                      "team=ops, example.com/tier=build",
		"OF_K8S_ANNOTATIONS":                 "cluster-autoscaler.kubernetes.io/safe-to-evict=false,note=a b c",
		"OF_K8S_IMAGE_PULL_POLICY":           "Always",
		"OF_K8S_RUN_AS_USER":                 "2000",
		"OF_K8S_WORKSPACE_SIZE_LIMIT":        "20Gi",
		"OF_K8S_PREFLIGHT_JOB":               "true",
		"OF_K8S_CONTROL_PLANE_NAMESPACE":     "of",
		"OF_K8S_CONTROL_PLANE_SELECTOR":      "app.kubernetes.io/name=openfactory",
		"OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE":  "example.test/runner:1",
		"OF_ALLOW_UNENFORCED_NETWORK_POLICY": "true",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.K8sLabelMap(), map[string]string{"team": "ops", "example.com/tier": "build"}) {
		t.Errorf("labels = %v", c.K8sLabelMap())
	}
	if c.K8sAnnotationMap()["cluster-autoscaler.kubernetes.io/safe-to-evict"] != "false" || c.K8sAnnotationMap()["note"] != "a b c" {
		t.Errorf("annotations = %v", c.K8sAnnotationMap())
	}
	if c.K8sImagePullPolicyValue() != "Always" || !c.K8sPreflightJob || !c.AllowUnenforcedNetworkPolicy {
		t.Errorf("unexpected values: %+v", c)
	}
	if u, img := c.K8sRunAsUserValue(); u != 2000 || img {
		t.Errorf("user = %d, %v", u, img)
	}
	if c.K8sEgressProxySidecarImage != "example.test/runner:1" {
		t.Errorf("sidecar image = %q", c.K8sEgressProxySidecarImage)
	}
}

func TestK8sRunAsImage(t *testing.T) {
	c, err := loadK8s(t, map[string]string{"OF_K8S_RUN_AS_USER": "image"})
	if err != nil {
		t.Fatal(err)
	}
	if u, img := c.K8sRunAsUserValue(); u != 0 || !img {
		t.Fatalf("user = %d, %v", u, img)
	}
}

func TestK8sValidation(t *testing.T) {
	bad := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"OF_K8S_IMAGE_PULL_POLICY": "Sometimes"}, "OF_K8S_IMAGE_PULL_POLICY"},
		{map[string]string{"OF_K8S_IMAGE_PULL_POLICY": "always"}, "OF_K8S_IMAGE_PULL_POLICY"},
		{map[string]string{"OF_K8S_LABELS": "novalue"}, "OF_K8S_LABELS"},
		{map[string]string{"OF_K8S_LABELS": "=x"}, "OF_K8S_LABELS"},
		{map[string]string{"OF_K8S_LABELS": "bad key=x"}, "OF_K8S_LABELS"},
		{map[string]string{"OF_K8S_LABELS": "a=has space"}, "OF_K8S_LABELS"},
		{map[string]string{"OF_K8S_LABELS": "a=" + strings.Repeat("x", 64)}, "OF_K8S_LABELS"},
		{map[string]string{"OF_K8S_ANNOTATIONS": "a"}, "OF_K8S_ANNOTATIONS"},
		{map[string]string{"OF_K8S_ANNOTATIONS": "-a=b"}, "OF_K8S_ANNOTATIONS"},
		{map[string]string{"OF_K8S_RUN_AS_USER": "root"}, "OF_K8S_RUN_AS_USER"},
		{map[string]string{"OF_K8S_RUN_AS_USER": "0"}, "OF_K8S_RUN_AS_USER"},
		{map[string]string{"OF_K8S_WORKSPACE_SIZE_LIMIT": "lots"}, "OF_K8S_WORKSPACE_SIZE_LIMIT"},
		{map[string]string{"OF_K8S_CONTROL_PLANE_NAMESPACE": "of"}, "OF_K8S_CONTROL_PLANE_NAMESPACE"},
		{map[string]string{"OF_K8S_PREFLIGHT_JOB": "maybe"}, "not a boolean"},
		{map[string]string{"OF_K8S_NETWORK_POLICY_CHECK": "sometimes"}, "OF_K8S_NETWORK_POLICY_CHECK"},
		{map[string]string{"OF_K8S_NETWORK_POLICY_ENFORCEMENT": "maybe"}, "OF_K8S_NETWORK_POLICY_ENFORCEMENT"},
		{map[string]string{"OF_K8S_NETWORK_POLICY_ENFORCEMENT": "require", "OF_K8S_NETWORK_POLICY_CHECK": "off"}, "needs OF_K8S_NETWORK_POLICY_CHECK=probe"},
		{map[string]string{"OF_K8S_EGRESS_LOCK": "nft"}, "OF_K8S_EGRESS_LOCK"},
		{map[string]string{"OF_K8S_EGRESS_LOCK": "iptables"}, "needs OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_TIMEOUT": "soon"}, "OF_K8S_EGRESS_WAIT_TIMEOUT"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_TIMEOUT": "0"}, "must be at least 1s"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_TIMEOUT": "-5s"}, "must be at least 1s"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_TIMEOUT": "200ms"}, "must be at least 1s"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_ADDR": "10.0.0.1"}, "OF_K8S_EGRESS_WAIT_ADDR"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_ADDR": ":10250"}, "OF_K8S_EGRESS_WAIT_ADDR"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_ADDR": "10.0.0.1:http"}, "OF_K8S_EGRESS_WAIT_ADDR"},
		{map[string]string{"OF_K8S_EGRESS_WAIT_ADDR": "10.0.0.1:70000"}, "OF_K8S_EGRESS_WAIT_ADDR"},
	}
	for _, tc := range bad {
		_, err := loadK8s(t, tc.env)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("settings %v: error %v, want containing %q", tc.env, err, tc.want)
		}
	}
}

func TestK8sNetworkPolicyDefaults(t *testing.T) {
	c, err := loadK8s(t, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if c.K8sNetworkPolicyCheckValue() != "probe" || c.K8sNetworkPolicyEnforcementValue() != "warn" || c.K8sEgressLockValue() != "none" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestK8sNetworkPolicySettings(t *testing.T) {
	c, err := loadK8s(t, map[string]string{
		"OF_K8S_NETWORK_POLICY_CHECK":       "OFF",
		"OF_K8S_NETWORK_POLICY_ENFORCEMENT": "warn",
		"OF_K8S_EGRESS_LOCK":                "IPTables",
		"OF_K8S_EGRESS_PROXY_SIDECAR_IMAGE": "example.test/of:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.K8sNetworkPolicyCheckValue() != "off" || c.K8sEgressLockValue() != "iptables" {
		t.Fatalf("unexpected values: %+v", c)
	}
	c, err = loadK8s(t, map[string]string{"OF_K8S_NETWORK_POLICY_ENFORCEMENT": "require"})
	if err != nil || c.K8sNetworkPolicyEnforcementValue() != "require" {
		t.Fatalf("require: %v %+v", err, c)
	}
}

func TestK8sEgressWaitDefaults(t *testing.T) {
	c, err := loadK8s(t, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	// 0 tells the backend to use its default time of 30 seconds.
	if c.K8sEgressWaitTimeoutValue() != 0 || c.K8sEgressWaitAddr != "" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestK8sEgressWaitSettings(t *testing.T) {
	for _, tc := range []struct {
		timeout string
		want    time.Duration
	}{
		{"", 0},
		{"45s", 45 * time.Second},
		{" 2m ", 2 * time.Minute},
		{"1s", time.Second},
		{"off", -1},
		{"OFF", -1},
	} {
		c, err := loadK8s(t, map[string]string{"OF_K8S_EGRESS_WAIT_TIMEOUT": tc.timeout})
		if err != nil {
			t.Fatalf("timeout %q: %v", tc.timeout, err)
		}
		if got := c.K8sEgressWaitTimeoutValue(); got != tc.want {
			t.Errorf("timeout %q = %v, want %v", tc.timeout, got, tc.want)
		}
	}
	c, err := loadK8s(t, map[string]string{
		"OF_K8S_EGRESS_WAIT_ADDR": "10.0.0.1:10250",
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.K8sEgressWaitAddr != "10.0.0.1:10250" {
		t.Fatalf("unexpected values: %+v", c)
	}
	if _, err := loadK8s(t, map[string]string{"OF_K8S_EGRESS_WAIT_ADDR": "[fd00::1]:443"}); err != nil {
		t.Fatalf("IPv6 target: %v", err)
	}
}
