package local

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/definition"
	"github.com/openfactory-org/openfactory/internal/forge/forgeutil/forgetest"
	"github.com/openfactory-org/openfactory/internal/forge/github"
	"github.com/openfactory-org/openfactory/internal/server"
	"github.com/openfactory-org/openfactory/pkg/sdk"
)

// scripts lists the shell files of the checks.
func scripts(t *testing.T) []string {
	t.Helper()
	libs, err := filepath.Glob("lib/*.sh")
	if err != nil || len(libs) == 0 {
		t.Fatalf("no lib/*.sh: %v", err)
	}
	return append([]string{"check.sh"}, libs...)
}

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, tool := range append([]string{"sh"}, tools...) {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on the PATH", tool)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// shell runs a script in sh with extra environment and returns stdout and stderr.
func shell(t *testing.T, script string, env ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestScriptsParse(t *testing.T) {
	needTools(t)
	for _, f := range scripts(t) {
		if out, err := exec.Command("sh", "-n", f).CombinedOutput(); err != nil {
			t.Errorf("sh -n %s: %v\n%s", f, err, out)
		}
	}
}

func TestShellcheck(t *testing.T) {
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Skip("shellcheck is not on the PATH")
	}
	// The lib files use variables that other files set, so SC2154 and friends do not apply.
	args := append([]string{"-s", "sh", "-S", "error", "-e", "SC2154,SC2034,SC1090,SC1091,SC2317,SC2329"}, scripts(t)...)
	if out, err := exec.Command("shellcheck", args...).CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

// Every check of the table in docs/self-hosting/local-testing.md has a function,
// a name, and a place in check.sh, and the other way round.
func TestEveryCheckIsDefined(t *testing.T) {
	docs := readFile(t, "../../docs/self-hosting/local-testing.md")
	row := regexp.MustCompile(`(?m)^\| ([A-Z]+[0-9]) \|`)
	var want []string
	for _, m := range row.FindAllStringSubmatch(docs, -1) {
		want = append(want, m[1])
	}
	if len(want) != 27 {
		t.Fatalf("the table of the page has %d checks, want 27: %v", len(want), want)
	}
	check := readFile(t, "check.sh")
	m := regexp.MustCompile(`(?m)^ALL_CHECKS="([^"]*)"`).FindStringSubmatch(check)
	if m == nil {
		t.Fatal("check.sh has no ALL_CHECKS line")
	}
	if got := strings.Fields(m[1]); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("ALL_CHECKS = %v\nthe table  = %v", got, want)
	}
	var libs strings.Builder
	for _, f := range scripts(t) {
		libs.WriteString(readFile(t, f))
	}
	for _, id := range want {
		if !strings.Contains(libs.String(), "\ncheck_"+id+"() {") {
			t.Errorf("no function check_%s", id)
		}
		if !regexp.MustCompile(`(?m)^\t` + id + `\) echo "`).MatchString(check) {
			t.Errorf("check_name has no entry for %s", id)
		}
	}
}

func TestWebhookTemplates(t *testing.T) {
	files, _ := filepath.Glob("webhooks/*.json")
	if len(files) < 2 {
		t.Fatalf("webhook templates: %v", files)
	}
	for _, f := range files {
		var v map[string]any
		if err := json.Unmarshal([]byte(readFile(t, f)), &v); err != nil {
			t.Errorf("%s is not JSON: %v", f, err)
			continue
		}
		repo, _ := v["repository"].(map[string]any)
		if repo["full_name"] == nil {
			t.Errorf("%s has no repository.full_name", f)
		}
		sender, _ := v["sender"].(map[string]any)
		if sender["type"] != "Bot" {
			t.Errorf("%s: the sender must be a bot, so that the unlinked-account rule does not drop the event", f)
		}
	}
	var il struct {
		Action string `json:"action"`
		Label  struct {
			Name string `json:"name"`
		} `json:"label"`
		Issue struct {
			Number int `json:"number"`
			Labels []struct {
				Name string `json:"name"`
			} `json:"labels"`
		} `json:"issue"`
	}
	if err := json.Unmarshal([]byte(readFile(t, "webhooks/issues-labeled.json")), &il); err != nil {
		t.Fatal(err)
	}
	if il.Action != "labeled" || !strings.HasPrefix(il.Label.Name, "factory:") || il.Issue.Number == 0 || len(il.Issue.Labels) == 0 {
		t.Errorf("issues-labeled.json is not a labeled issue with a factory label: %+v", il)
	}
}

// render fills in the placeholders of a factory tree with the function of the script.
func render(t *testing.T, name string) string {
	t.Helper()
	needTools(t)
	dst := filepath.Join(t.TempDir(), name)
	script := `HERE=$(pwd); . lib/common.sh; . lib/runs.sh; render_tree "factories/` + name + `" "$DST"`
	out, err := shell(t, script, "DST="+dst, "SANDBOX_IMAGE=ghcr.io/openfactory-org/sandbox:local", "GIT_CLONE_URL=git://git.of-local-infra.svc:9418/of-local-check.git", "LOCAL_DOMAIN=k8s.orb.local", "LOCAL_GITHUB_REPO=acme/widgets")
	if err != nil {
		t.Fatalf("render_tree %s: %v\n%s", name, err, out)
	}
	return dst
}

func TestFactoryDefinitionsCompile(t *testing.T) {
	dirs, _ := filepath.Glob("factories/*")
	if len(dirs) < 5 {
		t.Fatalf("factories: %v", dirs)
	}
	for _, d := range dirs {
		name := filepath.Base(d)
		t.Run(name, func(t *testing.T) {
			dir := render(t, name)
			files, err := definition.LoadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for p, c := range files {
				if strings.Contains(c, "__") {
					t.Errorf("%s still has a placeholder", p)
				}
			}
			def, verrs := definition.Compile(files)
			for _, e := range verrs {
				t.Errorf("%s:%d: %s", e.File, e.Line, e.Message)
			}
			if def == nil {
				t.Fatal("no definition")
			}
			for _, a := range def.Agents {
				// The agents run the claude CLI stand-in of the stack: the claude harness with a claude model.
				if string(a.Harness.Type) != "claude" || !strings.HasPrefix(a.Harness.Model, "claude-") {
					t.Errorf("agent %s: harness %s model %s, want the claude harness with a claude- model", a.Name, a.Harness.Type, a.Harness.Model)
				}
			}
			switch name {
			case "check":
				var coder bool
				for _, a := range def.Agents {
					if a.Name == "coder" && a.Machine != nil && a.Machine.Name == "git" && len(a.Machine.SetupCommands) == 1 &&
						strings.Contains(a.Machine.SetupCommands[0], "git://git.of-local-infra.svc:9418/of-local-check.git") &&
						strings.Contains(a.Machine.Image, "sandbox") {
						coder = true
					}
				}
				if !coder {
					t.Error("the agent coder does not use the machine that clones the test repository")
				}
				if def.Alias != "of-local-check" || len(def.Automations) != 1 {
					t.Errorf("alias %q, %d automation(s)", def.Alias, len(def.Automations))
				}
			case "net":
				for _, a := range def.Agents {
					if !a.NetworkPolicy.Restricted {
						t.Errorf("agent %s has no restricted network policy", a.Name)
					}
					if strings.Join(a.NetworkPolicy.AllowedHosts, ",") != "aws.k8s.orb.local" {
						t.Errorf("allowed hosts: %v", a.NetworkPolicy.AllowedHosts)
					}
				}
			case "gh":
				if len(def.Repositories) != 1 || def.Repositories[0].Owner != "acme" || def.Repositories[0].Name != "widgets" {
					t.Errorf("repositories: %+v", def.Repositories)
				}
			}
		})
	}
}

// Every kubectl call has the context, and the AWS CLI is used only through the
// connect.env of the stack. Nothing disables TLS verification.
func TestScriptRules(t *testing.T) {
	kubectlCall := regexp.MustCompile(`(^|[|;&(]|\$\()\s*kubectl\s`)
	awsCall := regexp.MustCompile(`(^|[|;&(]|\$\()\s*aws\s`)
	for _, f := range scripts(t) {
		for i, line := range strings.Split(readFile(t, f), "\n") {
			l := strings.TrimSpace(line)
			if strings.HasPrefix(l, "#") {
				continue
			}
			if kubectlCall.MatchString(l) && !strings.Contains(l, `kubectl --context "$KUBE_CONTEXT" "$@"`) {
				t.Errorf("%s:%d calls kubectl without kc: %s", f, i+1, l)
			}
			if awsCall.MatchString(l) && !strings.Contains(l, `. "$LOCAL_DIR/aws/connect.env" && aws "$@"`) {
				t.Errorf("%s:%d calls aws outside aws_emu: %s", f, i+1, l)
			}
			for _, bad := range []string{"--insecure", "curl -k", " -sk ", "--no-verify-ssl", "InsecureSkipVerify", "set -x", "--context orbstack", "--context=", "git daemon"} {
				if strings.Contains(l, bad) {
					t.Errorf("%s:%d: %q is not allowed: %s", f, i+1, bad, l)
				}
			}
		}
	}
}

// TestCheckPodImageHasCurl: the Pods that the checks start run curl, so their
// image is the testkit image of the stack, and its default stage installs curl.
// The runner image (the server image) has no curl.
func TestCheckPodImageHasCurl(t *testing.T) {
	m := regexp.MustCompile(`(?m)^check_pod_image\(\) \{ echo (\S+); \}$`).FindStringSubmatch(readFile(t, "lib/common.sh"))
	if m == nil {
		t.Fatal("lib/common.sh has no check_pod_image")
	}
	if !strings.Contains(readFile(t, "../../deploy/local/lib.sh"), "\nTESTKIT_IMAGE="+m[1]+"\n") {
		t.Errorf("check_pod_image prints %s, not TESTKIT_IMAGE of deploy/local/lib.sh", m[1])
	}
	df := readFile(t, "../../deploy/local/testkit/Dockerfile")
	last := df[strings.LastIndex(df, "\nFROM "):]
	if !regexp.MustCompile(`apk add --no-cache [^\n]*\bcurl\b`).MatchString(last) {
		t.Error("the default stage of deploy/local/testkit/Dockerfile does not install curl")
	}
	if !strings.Contains(readFile(t, "lib/runs.sh"), `_pod_image=$(check_pod_image)`) {
		t.Error("run_check_pod does not use check_pod_image")
	}
}

// fakeStack writes a minimal LOCAL_DIR and returns it.
func fakeStack(t *testing.T, server string, extra ...string) string {
	t.Helper()
	dir := t.TempDir()
	env := "export OF_SERVER=" + server + "\nexport OF_API_KEY=of_test_0123456789abcdef0123456789ab\nexport LOCAL_SERVER_MODE=host\nexport LOCAL_DIR=" + dir + "\n"
	for _, e := range extra {
		env += "export " + e + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runCheck(t *testing.T, dir string, env ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("sh", "check.sh")
	cmd.Env = append(os.Environ(), append([]string{"LOCAL_DIR=" + dir}, env...)...)
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return string(out), code
}

// The settings exclude checks: the lines are SKIP, with a reason, in the order
// of the table, and the result is PASS.
func TestFilterAndSkip(t *testing.T) {
	needTools(t)
	dir := fakeStack(t, "http://127.0.0.1:1")
	out, code := runCheck(t, dir, "LOCAL_CHECKS=g4,A2,r4,H2,A4,A3,G1")
	if code != 0 {
		t.Fatalf("exit status %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "Logs: "+dir) {
		t.Errorf("the first line is %q, want the log directory", lines[0])
	}
	var ids []string
	for _, l := range lines[1 : len(lines)-1] {
		f := strings.Fields(l)
		if f[0] != "SKIP" || !strings.Contains(l, ": needs ") {
			t.Errorf("not a SKIP line with a reason: %q", l)
		}
		ids = append(ids, f[1])
	}
	if got := strings.Join(ids, ","); got != "A2,A3,A4,R4,H2,G1,G4" {
		t.Errorf("order of the checks: %s", got)
	}
	if lines[len(lines)-1] != "RESULT: PASS" {
		t.Errorf("last line %q", lines[len(lines)-1])
	}
	if _, err := os.Stat(filepath.Join(dir, "check-logs")); err != nil {
		t.Errorf("no check-logs directory: %v", err)
	}
	// The forge is github, but the environment of the script lacks the repository and the token.
	dir = fakeStack(t, "http://127.0.0.1:1", "LOCAL_FORGE=github")
	out, _ = runCheck(t, dir, "LOCAL_CHECKS=G2", "LOCAL_GITHUB_REPO=", "GITHUB_TOKEN=")
	if !strings.Contains(out, "SKIP G2") || !strings.Contains(out, "LOCAL_GITHUB_REPO is not set") {
		t.Errorf("G2 without the repository: %s", out)
	}
	// A claude model enables R4: it is then not skipped (it fails here: no server).
	dir = fakeStack(t, "http://127.0.0.1:1", "LOCAL_MODEL=claude")
	out, code = runCheck(t, dir, "LOCAL_CHECKS=R4")
	if code != 1 || !strings.Contains(out, "FAIL R4 ") || !strings.HasSuffix(strings.TrimSpace(out), "RESULT: FAIL") {
		t.Errorf("R4 with LOCAL_MODEL=claude and no server: exit %d\n%s", code, out)
	}
}

func TestUnknownCheckAndMissingEnv(t *testing.T) {
	needTools(t)
	dir := fakeStack(t, "http://127.0.0.1:1")
	if out, code := runCheck(t, dir, "LOCAL_CHECKS=A1,Z9"); code != 2 || !strings.Contains(out, "unknown check 'Z9'") {
		t.Errorf("unknown ID: exit %d\n%s", code, out)
	}
	empty := t.TempDir()
	if out, code := runCheck(t, empty); code != 2 || !strings.Contains(out, "make local-up") {
		t.Errorf("no env file: exit %d\n%s", code, out)
	}
}

// A check that cannot reach the stack fails with one line, a reason and a log.
func TestFailLineAndLog(t *testing.T) {
	needTools(t, "curl", "jq")
	dir := fakeStack(t, "http://127.0.0.1:1")
	out, code := runCheck(t, dir, "LOCAL_CHECKS=P1")
	if code != 1 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !regexp.MustCompile(`(?m)^FAIL P1 Both server replicas are ready: .+`).MatchString(out) {
		t.Errorf("no FAIL line:\n%s", out)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "check-logs", "*", "P1.log"))
	if len(logs) != 1 || !strings.Contains(readFile(t, logs[0]), "FAIL ") {
		t.Errorf("log files: %v", logs)
	}
	summary, _ := filepath.Glob(filepath.Join(dir, "check-logs", "*", "summary.txt"))
	if len(summary) != 1 || !strings.Contains(readFile(t, summary[0]), "RESULT: FAIL") {
		t.Errorf("summary: %v", summary)
	}
}

func hmacHex(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func TestHookSignMatchesGo(t *testing.T) {
	needTools(t, "openssl")
	body := []byte(`{"zen":"x"}` + "\n")
	f := filepath.Join(t.TempDir(), "body.json")
	if err := os.WriteFile(f, body, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := shell(t, `. lib/common.sh; . lib/checks_hooks.sh; hook_sign "s3cr3t-value" "$F"`, "F="+f)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if want := "sha256=" + hmacHex("s3cr3t-value", body); out != want {
		t.Errorf("hook_sign = %q, want %q", out, want)
	}
}

// W1 and W2 against the real server, through check.sh. The checks write the
// webhook secret of the stack to a team secret, and the GitHub record of the
// owner checks the signatures with it (config.webhook_secret_ref).
func TestWebhookChecksAgainstTheHandler(t *testing.T) {
	needTools(t, "curl", "jq", "openssl")
	const secret = "of-local-test-webhook-secret-value"
	url, key := startServer(t)
	dir := fakeStack(t, url, "OF_API_KEY="+key)
	if err := os.WriteFile(filepath.Join(dir, "webhook.secret"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := runCheck(t, dir, "LOCAL_CHECKS=W1,W2")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	for _, want := range []string{"PASS W1 ", "PASS W2 ", "RESULT: PASS"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// A record with another secret: W1 fails, and the logs hold no secret.
	// The fake API answers the calls of fx_webhook_integration, and the record
	// it shows already names the team secret, so the checks change nothing.
	store := forgetest.NewStore()
	store.Integrations = []core.Integration{{ID: "int_w", TeamID: "team_1", Provider: "github", SecretRef: "GITHUB_TOKEN",
		Config: map[string]string{"org": "of-local-checks", "webhook_secret_ref": "LOCAL_WEBHOOK_SECRET"}}}
	srv2 := httptest.NewServer(func() http.Handler {
		m := http.NewServeMux()
		github.NewWebhookHandler(github.WebhookOptions{Store: store,
			Secrets: forgetest.NewSecrets(map[string]string{"LOCAL_WEBHOOK_SECRET": "another-secret-value-1234"})}).Register(m)
		m.HandleFunc("GET /api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"name":"`+r.PathValue("name")+`"}`)
		})
		m.HandleFunc("PUT /api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"name":"`+r.PathValue("name")+`"}`)
		})
		m.HandleFunc("GET /api/v1/integrations", func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": store.Integrations})
		})
		return m
	}())
	defer srv2.Close()
	dir2 := fakeStack(t, srv2.URL)
	if err := os.WriteFile(filepath.Join(dir2, "webhook.secret"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = runCheck(t, dir2, "LOCAL_CHECKS=W1")
	if code != 1 || !strings.Contains(out, "FAIL W1 ") || !strings.Contains(out, "401") {
		t.Errorf("W1 against a record with another secret: exit %d\n%s", code, out)
	}
	logs, _ := filepath.Glob(filepath.Join(dir2, "check-logs", "*", "*"))
	for _, l := range logs {
		c := readFile(t, l)
		for _, s := range []string{secret, "of_test_0123456789abcdef0123456789ab"} {
			if strings.Contains(c, s) {
				t.Errorf("%s holds a secret: %q", l, s)
			}
		}
	}
}

func TestMaskFile(t *testing.T) {
	needTools(t, "awk")
	f := filepath.Join(t.TempDir(), "log.txt")
	content := "key of_test_0123456789abcdef0123456789ab and webhook-secret-123456 and db-password-98765\n" +
		"url https://aws.k8s.orb.local:9443/b/k?X-Amz-Signature=0123abcd&X-Amz-Credential=AKIA%2F2026&X-Amz-Date=1\n" +
		"Authorization: Bearer abcdef.ghijkl\nsha256=" + strings.Repeat("a", 64) + "\nplain line\n"
	if err := os.WriteFile(f, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := shell(t, `. lib/common.sh; mask_file "$F"; cat "$F"`, "F="+f,
		"OF_API_KEY=of_test_0123456789abcdef0123456789ab", "MASK_WEBHOOK=webhook-secret-123456", "MASK_DB=db-password-98765")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, leak := range []string{"of_test_0123456789abcdef0123456789ab", "webhook-secret-123456", "db-password-98765", "0123abcd", "AKIA", "abcdef.ghijkl", strings.Repeat("a", 64)} {
		if strings.Contains(out, leak) {
			t.Errorf("the masked file still has %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "plain line") || !strings.Contains(out, "X-Amz-Date=1") {
		t.Errorf("the mask removed too much:\n%s", out)
	}
}

func TestHelperFunctions(t *testing.T) {
	needTools(t, "jq")
	for in, want := range map[string]string{"1h30m": "5400", "15m": "900", "720h": "2592000", "90s": "90", "2h": "7200"} {
		out, err := shell(t, `. lib/common.sh; . lib/checks_runs.sh; duration_seconds "$D"`, "D="+in)
		if err != nil || strings.TrimSpace(out) != want {
			t.Errorf("duration_seconds %s = %q (%v), want %s", in, out, err, want)
		}
	}
	out, err := shell(t, `. lib/common.sh; iso_to_epoch "2026-11-14T10:00:00.123456789Z"`)
	if err != nil || strings.TrimSpace(out) != "1794650400" {
		t.Errorf("iso_to_epoch = %q (%v)", out, err)
	}
	// tool_directive builds the text that the claude CLI stand-in reads.
	out, err = shell(t, `. lib/common.sh; . lib/runs.sh; tool_directive bash '{"command":"echo hi"}'`)
	if err != nil || out != `[[tool: bash {"command":"echo hi"}]]` {
		t.Errorf("tool_directive = %q (%v)", out, err)
	}
}

// H1 checks that the runner lives on after the turn: the run did not fail,
// its sandbox runs, and in chart mode a task Pod of the run is Running.
func TestH1RunAliveAfterTheTurn(t *testing.T) {
	needTools(t, "jq", "awk")
	script := `
WORK=$(mktemp -d); RESULT_FILE=$WORK/result
. lib/common.sh; . lib/runs.sh; . lib/checks_ha.sh
run_get() { RUN_JSON=$RUN; RUN_STATE=$(printf '%s' "$RUN" | jq -r '.state // empty'); }
task_pods() { [ -z "$PODS" ] || printf '%s\n' "$PODS"; }
if h_run_alive run_1; then echo "alive"; else echo "dead: $H_ALIVE_WHY"; fi
( h_hold_alive run_1 0; echo held ) >/dev/null 2>&1
cat "$RESULT_FILE" 2>/dev/null || echo "no result"
`
	running := `{"state":"SUCCEEDED","sandbox_running":true}`
	pod := "task-1 run_1.1 Running"
	for _, c := range []struct {
		name, mode, run, pods, want, result string
	}{
		{"chart, pod running", "chart", running, pod, "alive", "no result"},
		{"host, no pod check", "host", running, "", "alive", "no result"},
		{"run failed", "chart", `{"state":"FAILED","sandbox_running":false}`, pod, "dead: the run is FAILED", "FAIL\tafter the turn: the run is FAILED"},
		{"runner stopped", "chart", `{"state":"RUNNING","sandbox_running":false}`, pod, "dead: the run has no running sandbox", "FAIL\tafter the turn: the run has no running sandbox"},
		{"task Pod gone", "chart", running, "task-1 run_1.1 Failed", "dead: no task Pod of the run is Running", "FAIL\tafter the turn: no task Pod"},
		{"no task Pod", "chart", running, "", "dead: no task Pod of the run is Running", "FAIL\tafter the turn: no task Pod"},
		{"run unreadable", "chart", `{}`, pod, "dead: cannot read the run", "FAIL\tafter the turn: cannot read the run"},
	} {
		out, err := shell(t, script, "LOCAL_SERVER_MODE="+c.mode, "RUN="+c.run, "PODS="+c.pods)
		if err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		lines := strings.SplitN(strings.TrimSpace(out), "\n", 2)
		if len(lines) != 2 || !strings.HasPrefix(lines[0], c.want) || !strings.HasPrefix(lines[1], c.result) {
			t.Errorf("%s: got\n%s\nwant %q then %q", c.name, out, c.want, c.result)
		}
	}
}

// The run helpers against a small mock of the REST API: the dispatch body, the
// API key header, and the wait for a message of a role.
func TestRunHelpersAgainstMockAPI(t *testing.T) {
	needTools(t, "curl", "jq")
	var dispatched map[string]any
	var auth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/factories/fac_1/runs", func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&dispatched)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"run_id":"run_1","state":"QUEUED"}`))
	})
	mux.HandleFunc("GET /api/v1/runs/run_1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"run_id":"run_1","state":"SUCCEEDED","sandbox_running":false,"session":1}`))
	})
	mux.HandleFunc("GET /api/v1/runs/run_1/messages", func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after")
		msgs := `[{"seq":1,"role":"user","content":"hi"},{"seq":2,"role":"tool_result","content":"PUSHED-abc\n[exit code: 0]"},{"seq":3,"role":"assistant","content":"DONE"}]`
		if after == "3" {
			msgs = `[]`
		}
		_, _ = w.Write([]byte(`{"run_id":"run_1","messages":` + msgs + `,"has_more":false}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	script := `
HERE=$(pwd); WORK=$(mktemp -d); STATE_DIR=$WORK; CHECK_LOG=$WORK/log; CHECK_ID=T1; RESULT_FILE=$WORK/result
. lib/common.sh; . lib/runs.sh
run_dispatch fac_1 "do it" coder '{"idle_timeout_minutes":0}'
echo "RUN=$RUN_ID"
wait_reply "$RUN_ID" 0 assistant '^DONE$' 5; echo "rc=$? seq=$WR_SEQ"
wait_reply "$RUN_ID" 0 tool_result 'PUSHED-abc' 5; echo "rc=$? seq=$WR_SEQ"
wait_reply "$RUN_ID" 3 assistant 'DONE' 3; echo "rc=$?"
echo "last=$(last_seq "$RUN_ID")"
run_get "$RUN_ID"; echo "state=$RUN_STATE"
`
	out, err := shell(t, script, "OF_SERVER="+srv.URL, "OF_API_KEY=of_test_key_abcdef", "LOCAL_DOMAIN=k8s.orb.local", "LOCAL_DIR="+t.TempDir())
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"RUN=run_1", "rc=0 seq=3", "rc=0 seq=2", "rc=1", "last=3", "state=SUCCEEDED"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if auth != "Bearer of_test_key_abcdef" {
		t.Errorf("Authorization = %q", auth)
	}
	ov, _ := dispatched["overrides"].(map[string]any)
	if dispatched["agent"] != "coder" || ov["idle_timeout_minutes"] != float64(0) || dispatched["prompt"] != "do it" {
		t.Errorf("dispatch body: %v", dispatched)
	}
	md, _ := dispatched["metadata"].(map[string]any)
	if md["check"] != "T1" {
		t.Errorf("metadata: %v", dispatched["metadata"])
	}
}

// mockRuns is a small stand-in for the run API: it answers like a run with the
// claude CLI stand-in does. Each [[tool: bash {...}]] directive gives a tool call and a
// tool result, the turn ends with DONE (or READY without a directive), the
// session ends after the turn (idle timeout 0), and a message starts a new
// session that restores the workspace.
type mockRuns struct {
	files       map[string]string // the workspace snapshot of the run
	messages    []map[string]any
	timeline    []map[string]any
	session     int
	attachments map[string]string
}

func (m *mockRuns) add(role, content string) {
	m.messages = append(m.messages, map[string]any{"seq": len(m.messages) + 1, "role": role, "content": content})
}

var directive = regexp.MustCompile(`\[\[tool: (\w+) (\{.*?\})\]\]`)

func (m *mockRuns) turn(text string) {
	m.session++
	m.timeline = append(m.timeline, map[string]any{"type": "session.started", "session": m.session})
	if m.session > 1 {
		m.timeline = append(m.timeline, map[string]any{"type": "snapshot.restored", "session": m.session})
	}
	m.add("user", text)
	ds := directive.FindAllStringSubmatch(text, -1)
	for _, d := range ds {
		m.add("tool_call", d[1]+" "+d[2])
		var args struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal([]byte(d[2]), &args)
		out := "(no output)"
		switch {
		case strings.Contains(args.Command, "echo R2FILE="):
			tok := regexp.MustCompile(`R2FILE=(\w+)`).FindStringSubmatch(args.Command)[1]
			m.files["r2-note.txt"] = "R2FILE=" + tok
			out = "WROTE-" + regexp.MustCompile(`WROTE-(\w+)`).FindStringSubmatch(args.Command)[1]
		case strings.HasPrefix(args.Command, "cat r2-note.txt"):
			out = m.files["r2-note.txt"]
		case strings.HasPrefix(args.Command, "cat .openfactory/attachments/"):
			name := args.Command[strings.LastIndex(args.Command, "/")+1:]
			out = m.attachments[name]
		}
		m.add("tool_result", out)
	}
	if r := regexp.MustCompile(`\[\[reply: ([^\]]*)\]\]`).FindStringSubmatch(text); r != nil {
		m.add("assistant", r[1])
	} else if len(ds) > 0 {
		m.add("assistant", "DONE")
	} else {
		m.add("assistant", "READY")
	}
	m.timeline = append(m.timeline, map[string]any{"type": "snapshot.saved", "session": m.session})
}

func TestRunChecksAgainstMockRuns(t *testing.T) {
	needTools(t, "curl", "jq")
	m := &mockRuns{files: map[string]string{}, attachments: map[string]string{}}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, code int, v any) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /api/v1/factories/fac_1/runs", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		*m = mockRuns{files: map[string]string{}, attachments: m.attachments}
		m.turn(body.Prompt)
		writeJSON(w, http.StatusCreated, map[string]any{"run_id": "run_1"})
	})
	mux.HandleFunc("POST /api/v1/attachments", func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1000)
		n, _ := r.Body.Read(b)
		name := r.URL.Query().Get("name")
		m.attachments[name] = strings.TrimSpace(string(b[:n]))
		writeJSON(w, http.StatusCreated, map[string]any{"attachment_id": "att_" + name, "name": name})
	})
	mux.HandleFunc("GET /api/v1/runs/run_1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"run_id": "run_1", "state": "SUCCEEDED", "sandbox_running": false, "session": m.session, "session_id": "ses_standin"})
	})
	mux.HandleFunc("GET /api/v1/runs/run_1/messages", func(w http.ResponseWriter, r *http.Request) {
		var after int
		after, _ = strconv.Atoi(r.URL.Query().Get("after"))
		var out []map[string]any
		for _, x := range m.messages {
			if x["seq"].(int) > after {
				out = append(out, x)
			}
		}
		writeJSON(w, 200, map[string]any{"run_id": "run_1", "messages": out, "has_more": false})
	})
	mux.HandleFunc("POST /api/v1/runs/run_1/messages", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.turn(body.Message)
		writeJSON(w, 202, map[string]any{"run_id": "run_1", "message_id": "m1", "outcome": "new_session"})
	})
	mux.HandleFunc("GET /api/v1/runs/run_1/timeline", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"items": m.timeline})
	})
	mux.HandleFunc("GET /api/v1/runs/run_1/sessions", func(w http.ResponseWriter, r *http.Request) {
		var items []map[string]any
		for i := 1; i <= m.session; i++ {
			items = append(items, map[string]any{"session": i, "number": i, "snapshot": map[string]any{"workspace": true, "harness_state": true}})
		}
		writeJSON(w, 200, map[string]any{"items": items})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := fakeStack(t, srv.URL)
	// The same check.sh, with the parts that need the cluster replaced by stubs.
	script := `
set -u
HERE=$(pwd); REPO=$(cd ../.. && pwd)
BASE=$(mktemp -d); WORK=$BASE/w; STATE_DIR=$BASE/state; mkdir -p "$WORK" "$STATE_DIR"
CHECK_LOG=$BASE/log; RESULT_FILE=$BASE/result; CHECK_ID=$ID
. lib/common.sh; . lib/runs.sh; . lib/checks_runs.sh
LOCAL_SERVER_MODE=chart
fx_check_factory() { FACTORY_UID=fac_1; }
aws_guard() { :; }
task_pods() { :; }
server_env_value() { echo 1h; }
bucket_keys() { printf 'snapshots/team/run_1/1/workspace/manifest.json\nattachments/team/%s/%s\nruns/other\n' "${_r3_id:-}" "${_r3_name:-}"; }
need_cmd() { :; }
` + "( check_$ID ) >$BASE/out 2>&1; cat $BASE/result; echo; tail -n 25 $BASE/out | sed 's/^/log: /'"
	run := func(id string, extra ...string) string {
		out, err := shell(t, script, append([]string{"ID=" + id, "OF_SERVER=" + srv.URL, "OF_API_KEY=of_test_key_abcdef", "LOCAL_DOMAIN=k8s.orb.local", "LOCAL_DIR=" + dir, "KUBE_CONTEXT=orbstack"}, extra...)...)
		if err != nil {
			t.Fatalf("check %s: %v\n%s", id, err, out)
		}
		return out
	}
	if out := run("R2"); !strings.HasPrefix(out, "PASS\t") {
		t.Errorf("R2 against the mock:\n%s", out)
	}
	// R3 with no earlier attachment: the retention part cannot be observed, so it fails with that reason.
	if out := run("R3"); !strings.HasPrefix(out, "FAIL\t") || !strings.Contains(out, "retention is not observed yet") {
		t.Errorf("R3 without an overdue attachment:\n%s", out)
	}
	// R3 with an attachment of an earlier check that is older than the retention and gone: PASS.
	state := filepath.Join(dir, "check-state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "r3-attachments"), []byte("att_old 1000000000 r3-old.txt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run("R3"); !strings.HasPrefix(out, "PASS\t") {
		t.Errorf("R3 with an overdue attachment that is gone:\n%s", out)
	}
}

// startServer runs the real server in this process (memory store) and returns
// its URL and an admin API key.
func startServer(t *testing.T) (string, string) {
	t.Helper()
	ctx := t.Context()
	c := config.Defaults()
	c.DataDir = t.TempDir()
	c.DatabaseURL = "sqlite://:memory:"
	c.SandboxBackend = config.BackendLocal
	c.SandboxRoot = t.TempDir()
	c.SecretKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	ts := httptest.NewUnstartedServer(nil)
	c.PublicURL = "http://" + ts.Listener.Addr().String()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := server.New(ctx, &c, server.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		server.WithPollInterval(100*time.Millisecond), server.WithDefinitionSyncInterval(-1))
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()
	ts.Start()
	t.Cleanup(ts.Close)
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	boot, err := sdk.NewClient(ts.URL, "", sdk.WithMaxRetries(0)).Platform.Bootstrap(ctx, "of-local", "admin@of-local.test", "Admin")
	if err != nil {
		t.Fatal(err)
	}
	return ts.URL, boot.Key
}

// D1 and the dashboard part of UI1 against the real server, with the real CLI.
func TestDefinitionAndDashboardChecksAgainstTheServer(t *testing.T) {
	needTools(t, "curl", "jq", "go")
	url, key := startServer(t)
	bin := filepath.Join(t.TempDir(), "openfactory")
	build := exec.Command("go", "build", "-o", bin, "./cmd/openfactory")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dir := fakeStack(t, url)
	script := `
set -u
HERE=$(pwd); REPO=$(cd ../.. && pwd)
BASE=$(mktemp -d); WORK=$BASE/w; STATE_DIR=$BASE/state; mkdir -p "$WORK" "$STATE_DIR"
CHECK_LOG=$BASE/log; RESULT_FILE=$BASE/result; CHECK_ID=D1
. lib/common.sh; . lib/runs.sh; . lib/checks_platform.sh
ensure_cli() { OFBIN=$BIN; }
( check_D1 ) >$BASE/out 2>&1; cat $BASE/result; echo; tail -n 30 $BASE/out | sed 's/^/log: /'
`
	env := []string{"BIN=" + bin, "OF_SERVER=" + url, "OF_API_KEY=" + key, "LOCAL_DOMAIN=k8s.orb.local", "LOCAL_DIR=" + dir}
	for i := 1; i <= 2; i++ { // the second run must work on the state that the first one leaves
		out, err := shell(t, script, env...)
		if err != nil {
			t.Fatalf("D1 run %d: %v\n%s", i, err, out)
		}
		if !strings.HasPrefix(out, "PASS\t") {
			t.Fatalf("D1 run %d against the server:\n%s", i, out)
		}
	}
}

// UI1 against the real server: the app shell, the session login and the API
// reads of the pages.
func TestDashboardCheckAgainstTheServer(t *testing.T) {
	needTools(t, "curl", "jq")
	ctx := t.Context()
	url, key := startServer(t)
	cl := sdk.NewClient(url, key, sdk.WithMaxRetries(0))
	created, err := cl.Factories.Create(ctx, sdk.CreateFactoryParams{Name: "of-local-check", Alias: "of-local-check"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := cl.Factories.Dispatch(ctx, created.Factory.UID, sdk.DispatchParams{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	dir := fakeStack(t, url)
	script := `
set -u
HERE=$(pwd); REPO=$(cd ../.. && pwd)
BASE=$(mktemp -d); WORK=$BASE/w; STATE_DIR=$BASE/state; mkdir -p "$WORK" "$STATE_DIR"
CHECK_LOG=$BASE/log; RESULT_FILE=$BASE/result; CHECK_ID=UI1
. lib/common.sh; . lib/runs.sh; . lib/checks_platform.sh
fx_artifact_run() { ARTIFACT_RUN=$RUN; ARTIFACT_NAME=none; echo "$RUN" >>"$STATE_DIR/runs"; }
( check_UI1 ) >$BASE/out 2>&1; cat $BASE/result; echo; tail -n 30 $BASE/out | sed 's/^/log: /'
`
	out, err := shell(t, script, "RUN="+run.RunID, "OF_SERVER="+url, "OF_API_KEY="+key, "LOCAL_DOMAIN=k8s.orb.local", "LOCAL_DIR="+dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "PASS\t") {
		t.Fatalf("UI1 against the server:\n%s", out)
	}
}

// W3 against the real server and the real webhook handler: the signed delivery
// makes a work item and a run, and the same delivery again makes nothing new.
// There is no model in this test, so the wait for the first turn is replaced.
func TestWebhookDedupeAgainstTheServer(t *testing.T) {
	needTools(t, "curl", "jq", "openssl", "go")
	const secret = "of-local-test-webhook-secret-value"
	url, key := startServer(t)
	bin := filepath.Join(t.TempDir(), "openfactory")
	build := exec.Command("go", "build", "-o", bin, "./cmd/openfactory")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	dir := fakeStack(t, url)
	if err := os.WriteFile(filepath.Join(dir, "webhook.secret"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The check reads the sandbox image from the server settings. Without
	// them it looks for a local Docker image, which CI does not have.
	if err := os.WriteFile(filepath.Join(dir, "server-host.env"), []byte("export OF_SANDBOX_IMAGE=example.test/sandbox:test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `
set -u
HERE=$(pwd); REPO=$(cd ../.. && pwd)
BASE=$(mktemp -d); WORK=$BASE/w; STATE_DIR=$BASE/state; mkdir -p "$WORK" "$STATE_DIR"
CHECK_LOG=$BASE/log; RESULT_FILE=$BASE/result; CHECK_ID=W3
. lib/common.sh; . lib/runs.sh; . lib/checks_hooks.sh
ensure_cli() { OFBIN=$BIN; }
expect_reply() { WR_SEQ=0; }
( check_W3 ) >$BASE/out 2>&1; cat $BASE/result; echo; tail -n 40 $BASE/out | sed 's/^/log: /'
`
	out, err := shell(t, script, "BIN="+bin, "OF_SERVER="+url, "OF_API_KEY="+key, "LOCAL_DOMAIN=k8s.orb.local", "LOCAL_DIR="+dir)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "PASS\t") {
		t.Fatalf("W3 against the server:\n%s", out)
	}
}
