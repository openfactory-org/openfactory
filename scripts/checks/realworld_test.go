package checks

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// dryRun runs "real-world.sh harness-turn claude" in dry-run mode and returns
// the request body that the script prints.
func dryRun(t *testing.T, env ...string) map[string]any {
	t.Helper()
	for _, tool := range []string{"sh", "curl", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on the PATH", tool)
		}
	}
	cmd := exec.Command("sh", "real-world.sh", "harness-turn", "claude")
	cmd.Env = append(os.Environ(), "OF_API_KEY=test", "OF_CHECK_FACTORY=f", "OF_CHECK_DRY_RUN=1",
		"OF_CHECK_CLAUDE_SECRET=", "OF_CHECK_CLAUDE_MODEL=", "OF_CHECK_RESUME=")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s", err, out)
	}
	i := strings.Index(string(out), "{")
	if i < 0 {
		t.Fatalf("no request body in the output:\n%s", out)
	}
	var body map[string]any
	if err := json.Unmarshal(out[i:], &body); err != nil {
		t.Fatalf("the request body is not JSON: %v\n%s", err, out)
	}
	return body
}

// A typed harness override replaces the harness of the agent, with its auth.
// Without a secret or a model the script must not send one.
func TestHarnessTurnSendsNoHarnessOverrideByDefault(t *testing.T) {
	body := dryRun(t)
	if ov, ok := body["overrides"].(map[string]any); ok {
		if _, has := ov["harness"]; has {
			t.Fatalf("overrides has a harness without a secret or a model: %v", ov)
		}
	}
}

func TestHarnessTurnResumeSetsIdleTimeout(t *testing.T) {
	body := dryRun(t, "OF_CHECK_RESUME=1")
	ov, _ := body["overrides"].(map[string]any)
	if ov["idle_timeout_minutes"] != float64(1) {
		t.Fatalf("idle_timeout_minutes is not 1: %v", ov)
	}
	if _, has := ov["harness"]; has {
		t.Fatalf("overrides has a harness: %v", ov)
	}
}

func TestHarnessTurnSecretAndModelOverrideHarness(t *testing.T) {
	body := dryRun(t, "OF_CHECK_CLAUDE_SECRET=KEY", "OF_CHECK_CLAUDE_MODEL=m1")
	ov, _ := body["overrides"].(map[string]any)
	h, _ := ov["harness"].(map[string]any)
	if h["type"] != "claude" || h["model"] != "m1" {
		t.Fatalf("harness: %v", h)
	}
	auth, _ := h["auth"].(map[string]any)
	if auth["source"] != "managedSecret" || auth["secretName"] != "KEY" {
		t.Fatalf("auth: %v", auth)
	}
}
