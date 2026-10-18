package egress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// memAPI is an in-memory stand-in for the Kubernetes API of the probe. The
// scenario sets how the client Pods end.
type memAPI struct {
	mu       sync.Mutex
	pods     map[string]map[string]any
	policies map[string]map[string]any
	deletes  []string
	order    []string

	controlExit, deniedExit int    // exit code of the client Pods
	recheckExit             *int   // exit code of the recheck client; nil: as the denied client
	targetWaiting           string // waiting reason of the target Pod
	createErr               error
}

func newMemAPI() *memAPI {
	return &memAPI{pods: map[string]map[string]any{}, policies: map[string]map[string]any{}}
}

func (m *memAPI) do(_ context.Context, method, path string, body, out any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	isPod := strings.Contains(path, "/pods")
	name := path[strings.LastIndex(path, "/")+1:]
	switch method {
	case http.MethodPost:
		if m.createErr != nil {
			return m.createErr
		}
		obj := body.(map[string]any)
		meta := obj["metadata"].(map[string]any)
		n := meta["name"].(string)
		if isPod {
			m.pods[n] = obj
		} else {
			m.policies[n] = obj
		}
		m.order = append(m.order, n)
		return nil
	case http.MethodDelete:
		m.deletes = append(m.deletes, name)
		if isPod {
			delete(m.pods, name)
		} else {
			delete(m.policies, name)
		}
		return nil
	case http.MethodGet:
		p, ok := m.pods[name]
		if !ok {
			return fmt.Errorf("pod %s: %w", name, core.ErrNotFound)
		}
		role := p["metadata"].(map[string]any)["labels"].(map[string]any)[labelProbeRole].(string)
		var st map[string]any
		switch role {
		case roleTarget:
			if m.targetWaiting != "" {
				st = map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{
					"state": map[string]any{"waiting": map[string]any{"reason": m.targetWaiting, "message": "pull failed"}}}}}
			} else {
				st = map[string]any{"phase": "Running", "podIP": "10.0.0.5",
					"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
			}
		default:
			code := m.controlExit
			if role == roleDenied {
				code = m.deniedExit
				if strings.HasSuffix(name, "-"+roleRecheck) && m.recheckExit != nil {
					code = *m.recheckExit
				}
			}
			phase := "Succeeded"
			if code != 0 {
				phase = "Failed"
			}
			st = map[string]any{"phase": phase, "containerStatuses": []any{map[string]any{
				"state": map[string]any{"terminated": map[string]any{"exitCode": code}}}}}
		}
		raw, _ := json.Marshal(map[string]any{"status": st})
		return json.Unmarshal(raw, out)
	}
	return fmt.Errorf("unexpected %s %s", method, path)
}

func (m *memAPI) params() ProbeParams {
	return ProbeParams{
		Do: m.do, Namespace: "sandboxes", Image: "example.test/of:1", PullPolicy: "IfNotPresent",
		Poll: time.Millisecond, Timeout: 5 * time.Second, PullGrace: time.Millisecond, DeniedDelay: time.Second,
	}
}

func TestProbeEnforced(t *testing.T) {
	m := newMemAPI()
	m.deniedExit = 1 // the denied client could not connect
	got, err := Probe(context.Background(), m.params())
	if err != nil || got != Enforced {
		t.Fatalf("got %v, %v", got, err)
	}
	// Everything is removed.
	if len(m.pods) != 0 || len(m.policies) != 0 {
		t.Errorf("left behind: %v %v", m.pods, m.policies)
	}
	if len(m.deletes) != 5 {
		t.Errorf("deletes = %v", m.deletes)
	}
}

func TestProbeNotEnforced(t *testing.T) {
	m := newMemAPI() // both clients connect
	got, err := Probe(context.Background(), m.params())
	if err != nil || got != NotEnforced {
		t.Fatalf("got %v, %v", got, err)
	}
}

// A busy network plugin applies the policy late: the first denied client
// connects, the recheck client does not. The answer is Enforced, not a false
// "not enforced".
func TestProbeSlowPluginIsEnforced(t *testing.T) {
	m := newMemAPI() // the first denied client connects
	blocked := 1
	m.recheckExit = &blocked
	got, err := Probe(context.Background(), m.params())
	if err != nil || got != Enforced {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(m.pods) != 0 {
		t.Errorf("left behind: %v", m.pods)
	}
}

func TestProbeControlFailureHasNoAnswer(t *testing.T) {
	m := newMemAPI()
	m.controlExit, m.deniedExit = 1, 1
	got, err := Probe(context.Background(), m.params())
	if got != EnforcementUnknown || err == nil || !strings.Contains(err.Error(), "control Pod") {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestProbeImagePullError(t *testing.T) {
	m := newMemAPI()
	m.targetWaiting = "ImagePullBackOff"
	p := m.params()
	got, err := Probe(context.Background(), p)
	if got != EnforcementUnknown || err == nil || !strings.Contains(err.Error(), "ImagePullBackOff") {
		t.Fatalf("got %v, %v", got, err)
	}
	if len(m.pods) != 0 {
		t.Error("the target Pod is left behind")
	}
}

func TestProbeTimeoutCleansUp(t *testing.T) {
	m := newMemAPI()
	m.targetWaiting = "ContainerCreating"
	p := m.params()
	p.Timeout = 30 * time.Millisecond
	_, err := Probe(context.Background(), p)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if len(m.pods) != 0 {
		t.Error("the target Pod is left behind after a timeout")
	}
}

func TestProbeCreateErrorIsReported(t *testing.T) {
	m := newMemAPI()
	m.createErr = errors.New("forbidden")
	got, err := Probe(context.Background(), m.params())
	if got != EnforcementUnknown || err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestProbeObjects(t *testing.T) {
	m := newMemAPI()
	m.deniedExit = 1
	p := m.params()
	p.ImagePullSecrets = []string{"regcred"}
	p.Template = map[string]any{
		"tolerations":  []any{map[string]any{"key": "sandbox", "operator": "Exists"}},
		"nodeSelector": map[string]any{"pool": "agents"},
		"containers":   []any{map[string]any{"name": "sandbox"}},
	}
	var seen []map[string]any
	inner := p.Do
	p.Do = func(ctx context.Context, method, path string, body, out any) error {
		if method == http.MethodPost {
			seen = append(seen, body.(map[string]any))
		}
		return inner(ctx, method, path, body, out)
	}
	if _, err := Probe(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	pods := map[string]map[string]any{}
	for _, o := range seen {
		if o["kind"] == "NetworkPolicy" {
			policy = o
			continue
		}
		role := o["metadata"].(map[string]any)["labels"].(map[string]any)[labelProbeRole].(string)
		pods[role] = o
	}
	// The policy denies all egress for the denied client only.
	spec := policy["spec"].(map[string]any)
	if got := spec["egress"].([]any); len(got) != 0 {
		t.Errorf("egress = %v, want none", got)
	}
	if got := spec["policyTypes"].([]any); len(got) != 1 || got[0] != "Egress" {
		t.Errorf("policy types = %v", got)
	}
	sel := spec["podSelector"].(map[string]any)["matchLabels"].(map[string]any)
	if sel[labelProbeRole] != roleDenied {
		t.Errorf("selector = %v", sel)
	}
	for _, role := range []string{roleTarget, roleControl, roleDenied} {
		pod := pods[role]
		if pod == nil {
			t.Fatalf("no %s Pod", role)
		}
		ps := pod["spec"].(map[string]any)
		c := ps["containers"].([]any)[0].(map[string]any)
		cmd := c["command"].([]any)
		if cmd[0] != "openfactory" || cmd[1] != "netprobe" {
			t.Errorf("%s command = %v", role, cmd)
		}
		sc := c["securityContext"].(map[string]any)
		if sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false {
			t.Errorf("%s security context = %v", role, sc)
		}
		if ps["restartPolicy"] != "Never" || ps["automountServiceAccountToken"] != false {
			t.Errorf("%s spec = %v", role, ps)
		}
		if ps["imagePullSecrets"].([]any)[0].(map[string]any)["name"] != "regcred" {
			t.Errorf("%s pull secrets = %v", role, ps["imagePullSecrets"])
		}
		if len(ps["tolerations"].([]any)) != 1 {
			t.Errorf("%s tolerations = %v", role, ps["tolerations"])
		}
		ns := ps["nodeSelector"].(map[string]any)
		if ns["pool"] != "agents" || ns["kubernetes.io/os"] != "linux" {
			t.Errorf("%s node selector = %v", role, ns)
		}
	}
	// The target has a readiness probe. The denied client waits first.
	if pods[roleTarget]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["readinessProbe"] == nil {
		t.Error("the target has no readiness probe")
	}
	dcmd := fmt.Sprint(pods[roleDenied]["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)["command"])
	if !strings.Contains(dcmd, "--delay 1s") || !strings.Contains(dcmd, "10.0.0.5:8080") {
		t.Errorf("denied command = %s", dcmd)
	}
}

func TestChecker(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	c := NewChecker(func(context.Context) (Enforcement, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return Enforced, nil
	}, nil)
	if c.Cached() != EnforcementUnknown {
		t.Fatal("the checker must start with unknown")
	}
	var wg sync.WaitGroup
	results := make([]Enforcement, 3)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], _ = c.Result(context.Background())
		}()
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	for _, r := range results {
		if r != Enforced {
			t.Errorf("result = %v", r)
		}
	}
	if st, _ := c.Result(context.Background()); st != Enforced {
		t.Error("the answer is not kept")
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("the check ran %d times", calls)
	}
}

func TestCheckerCallerCanGiveUp(t *testing.T) {
	release := make(chan struct{})
	c := NewChecker(func(context.Context) (Enforcement, error) { <-release; return Enforced, nil }, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := c.Result(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	close(release)
	if st, err := c.Result(context.Background()); err != nil || st != Enforced {
		t.Fatalf("the check must go on after a caller gave up: %v, %v", st, err)
	}
}

func TestCheckerRetriesAfterAnError(t *testing.T) {
	calls := 0
	c := NewChecker(func(context.Context) (Enforcement, error) {
		calls++
		if calls == 1 {
			return EnforcementUnknown, errors.New("no image")
		}
		return NotEnforced, nil
	}, nil)
	c.RetryAfter = 30 * time.Millisecond
	if _, err := c.Result(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if _, err := c.Result(context.Background()); err == nil || calls != 1 {
		t.Fatalf("a failed check must not run again at once (calls %d, err %v)", calls, err)
	}
	time.Sleep(50 * time.Millisecond)
	if st, err := c.Result(context.Background()); err != nil || st != NotEnforced || calls != 2 {
		t.Fatalf("st %v err %v calls %d", st, err, calls)
	}
}

func TestGate(t *testing.T) {
	ctx := context.Background()
	mk := func(st Enforcement, err error) *Checker {
		return NewChecker(func(context.Context) (Enforcement, error) { return st, err }, nil)
	}
	tests := []struct {
		name    string
		c       *Checker
		mode    string
		allow   bool
		wantErr string
	}{
		{"warn never fails", mk(NotEnforced, nil), EnforcementWarn, false, ""},
		{"default mode is warn", mk(NotEnforced, nil), "", false, ""},
		{"require and enforced", mk(Enforced, nil), EnforcementRequire, false, ""},
		{"require and not enforced", mk(NotEnforced, nil), EnforcementRequire, false, "does not enforce NetworkPolicy"},
		{"require and no answer", mk(EnforcementUnknown, errors.New("boom")), EnforcementRequire, false, "boom"},
		{"require and check off", nil, EnforcementRequire, false, "OF_K8S_NETWORK_POLICY_CHECK=probe"},
		{"allow unenforced wins", mk(NotEnforced, nil), EnforcementRequire, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Gate(ctx, tc.c, tc.mode, tc.allow, nil, "task")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if !errors.Is(err, core.ErrInvalid) || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateSettings(t *testing.T) {
	for _, v := range []string{"", "off", "probe"} {
		if err := ValidateCheck(v); err != nil {
			t.Errorf("check %q: %v", v, err)
		}
	}
	for _, v := range []string{"", "warn", "require"} {
		if err := ValidateEnforcement(v); err != nil {
			t.Errorf("enforcement %q: %v", v, err)
		}
	}
	if ValidateCheck("x") == nil || ValidateEnforcement("x") == nil {
		t.Error("an unknown value must fail")
	}
}

func TestNetprobeServeAndDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeProbe(ctx, addr) }()
	if !DialProbe(ctx, addr, 0, 20, 20*time.Millisecond, time.Second) {
		t.Fatal("dial to a listening server failed")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve: %v", err)
	}
	// Nothing listens now.
	if DialProbe(context.Background(), addr, 0, 2, 5*time.Millisecond, 200*time.Millisecond) {
		t.Fatal("dial worked with no server")
	}
	// A cancelled context ends the wait.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if DialProbe(ctx2, addr, time.Hour, 1, 0, time.Second) {
		t.Fatal("a cancelled dial must fail")
	}
}
