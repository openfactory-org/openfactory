package naming

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// oldNames are the old product and machine names. Each is built from two
// parts so that this file does not contain it. The product is "Open Factory";
// the machine name, the Go module and the GitHub organization use
// "openfactory" and "openfactory-org".
var oldNames = [][]byte{
	[]byte("openfactor" + "ies"),
	[]byte("open-factor" + "ies"),
	[]byte("Open Factor" + "ies"),
}

func hasOldName(b []byte) bool {
	for _, n := range oldNames {
		if bytes.Contains(b, n) {
			return true
		}
	}
	return false
}

// TestOneMachineName fails when an old product or machine name is in a
// tracked or new file, or in its path.
func TestOneMachineName(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files is not available: %v", err)
	}
	var bad []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue // a deleted file that git still tracks, or a directory
		}
		if hasOldName([]byte(path)) {
			bad = append(bad, path+" (path)")
			continue
		}
		if bytes.IndexByte(data, 0) >= 0 {
			continue // binary
		}
		for i, line := range bytes.Split(data, []byte("\n")) {
			if hasOldName(line) {
				bad = append(bad, path+":"+strconv.Itoa(i+1))
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("use \"Open Factory\" (product) or \"openfactory\" (machine name), not %q. Found in:\n  %s",
			oldNames, strings.Join(bad, "\n  "))
	}
}

// glossaryScope lists the paths whose text users read. The code of the
// CLIs, the control plane and the runtime counts for its help strings and
// messages, so comment lines in code files are skipped. A directory that
// does not exist yet adds nothing.
var glossaryScope = []string{
	"docs/",
	"examples/",
	"web/src/",
	"cmd/openfactory-runtime/",
	"cmd/openfactory/",
	"apps/cli/",
	"apps/control-plane/src/",
	"deploy/sandbox/",
	"deploy/machine/",
}

// codeScope lists the directories whose files are code. Only some
// suffixes count there (see isCodeText), and comment lines are skipped.
var codeScope = []string{
	"cmd/openfactory-runtime/",
	"cmd/openfactory/",
	"apps/cli/",
	"apps/control-plane/src/",
}

// forbidden lists the words that the glossary replaced. Each entry
// gives the replacement in the message.
var forbidden = []struct {
	re  *regexp.Regexp
	use string
}{
	{regexp.MustCompile(`openfactory task\b`), "openfactory work-item"},
	{regexp.MustCompile(`\b(list_tasks|search_task|get_task|send_task|complete_task)\b`), "the *_work_item tools"},
	{regexp.MustCompile(`(?i)\btask backends?\b`), "sandbox provider"},
	{regexp.MustCompile(`(?i)\bsandbox backends?\b`), "sandbox provider"},
	{regexp.MustCompile(`(?i)\btask images?\b`), "sandbox image"},
	{regexp.MustCompile(`(?i)\btool images?\b`), "sandbox image"},
	{regexp.MustCompile(`\bdockerImage\b`), "image"},
	{regexp.MustCompile(`\bdocker_image\b`), "image"},
	{regexp.MustCompile(`openfactory mcp-server\b`), "openfactory mcp-registry"},
	{regexp.MustCompile(`workerHost: local\b`), "workerHost: server"},
	{regexp.MustCompile(`\brunners/`), "machines/"},
	// Removed platforms. Open Factory runs on Cloudflare, hosted sandbox
	// providers and microVM workers.
	{regexp.MustCompile(`(?i)\b(kubernetes|k8s|helm)\b`), "nothing: Open Factory has no Kubernetes or Helm support"},
	{regexp.MustCompile(`(?i)\b(aws|amazon|bedrock|eks|irsa)\b`), "nothing: Open Factory has no AWS support (name R2 as S3-compatible storage)"},
	{regexp.MustCompile(`\bopenfactory serve\b`), "wrangler deploy or wrangler dev"},
	{regexp.MustCompile(`\bOF_PLUGIN_DIR\b`), "plugin Workers"},
	{regexp.MustCompile(`\bopenfactory-plugin-`), "plugin Workers"},
	{regexp.MustCompile(`(?i)\breplicas?\b`), "nothing: there is one control plane"},
	{regexp.MustCompile(`\bOF_SANDBOX_BACKEND\b`), "OF_SANDBOX_PROVIDER"},
}

// isCodeText reports whether a file in a code directory has user text: the
// source of the CLIs, the control plane and the runtime. Tests, generated type files and fixtures do not count.
func isCodeText(path string) bool {
	switch {
	case strings.HasSuffix(path, "_test.go"),
		strings.HasSuffix(path, ".test.ts"),
		strings.HasSuffix(path, ".d.ts"),
		strings.Contains(path, "/test/"),
		strings.Contains(path, "/node_modules/"),
		strings.Contains(path, "/dist/"):
		return false
	}
	return strings.HasSuffix(path, ".go") || strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".sh")
}

func inCodeScope(path string) bool {
	for _, p := range codeScope {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func inGlossaryScope(path string) bool {
	for _, p := range glossaryScope {
		if strings.HasPrefix(path, p) {
			return !inCodeScope(path) || isCodeText(path)
		}
	}
	return false
}

// isCommentLine reports whether a code line is only a comment.
func isCommentLine(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "//") || strings.HasPrefix(t, "/*") || strings.HasPrefix(t, "*")
}

// TestGlossaryWords fails when user text has a word that the glossary
// replaced.
func TestGlossaryWords(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("git ls-files is not available: %v", err)
	}
	var bad []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" || !inGlossaryScope(path) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			continue // deleted, a directory, or binary
		}
		code := inCodeScope(path)
		for i, line := range strings.Split(string(data), "\n") {
			if code && isCommentLine(line) {
				continue
			}
			for _, f := range forbidden {
				if m := f.re.FindString(line); m != "" {
					bad = append(bad, fmt.Sprintf("%s:%d: %q (use %s)", path, i+1, m, f.use))
				}
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("user text has words that the glossary replaced (see docs/overview.md#glossary):\n  %s",
			strings.Join(bad, "\n  "))
	}
}
