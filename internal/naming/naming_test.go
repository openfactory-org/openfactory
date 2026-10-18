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

// glossaryScope lists the paths whose text users read. The Go files of the
// CLI count for their help strings, so comment lines there are skipped.
var glossaryScope = []string{
	"docs/",
	"examples/",
	"web/src/",
	"deploy/helm/",
	"cmd/openfactory/",
}

// forbidden lists the words that the glossary replaced. Each entry
// gives the replacement in the message.
var forbidden = []struct {
	re  *regexp.Regexp
	use string
}{
	{regexp.MustCompile(`openfactory task\b`), "openfactory work-item"},
	{regexp.MustCompile(`\b(list_tasks|search_task|get_task|send_task|complete_task)\b`), "the *_work_item tools"},
	{regexp.MustCompile(`(?i)\btask backends?\b`), "sandbox backend"},
	{regexp.MustCompile(`(?i)\btask images?\b`), "sandbox image"},
	{regexp.MustCompile(`\bdockerImage\b`), "image"},
	{regexp.MustCompile(`\bdocker_image\b`), "image"},
	{regexp.MustCompile(`openfactory mcp-server\b`), "openfactory mcp-registry"},
	{regexp.MustCompile(`workerHost: local\b`), "workerHost: server"},
	{regexp.MustCompile(`\brunners/`), "machines/"},
}

func inGlossaryScope(path string) bool {
	for _, p := range glossaryScope {
		if strings.HasPrefix(path, p) {
			return !strings.HasPrefix(path, "cmd/openfactory/") ||
				(strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go"))
		}
	}
	return false
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
		goFile := strings.HasSuffix(path, ".go")
		for i, line := range strings.Split(string(data), "\n") {
			if goFile && strings.HasPrefix(strings.TrimSpace(line), "//") {
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
