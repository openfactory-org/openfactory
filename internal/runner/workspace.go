package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	maxStepOutput = 16 << 10
	// maxFailureOutput is the part of the output of a failed step that goes
	// into the end message, and so into the run.
	maxFailureOutput    = 4000
	defaultSetupTimeout = 30 * time.Minute
)

// setupError is a failure of the workspace setup.
type setupError struct{ msg string }

func (e *setupError) Error() string { return e.msg }

// checkRelPath returns an error if p is empty, absolute or has a ".." part.
func checkRelPath(p string) error {
	if p == "" {
		return errors.New("path is empty")
	}
	if filepath.IsAbs(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) || filepath.VolumeName(p) != "" {
		return fmt.Errorf("path %q is absolute", p)
	}
	for _, part := range strings.FieldsFunc(p, func(c rune) bool { return c == '/' || c == '\\' }) {
		if part == ".." {
			return fmt.Errorf("path %q has a \"..\" part", p)
		}
	}
	return nil
}

// setup prepares the workspace: skills, repositories and setup commands. It
// returns a *setupError for a failure of a step. Each step sends events.
func (r *runner) setup(ctx context.Context) error {
	if err := os.MkdirAll(r.cfg.WorkDir, 0o755); err != nil {
		return &setupError{"create workspace: " + err.Error()}
	}
	timeout := defaultSetupTimeout
	if s := r.start.Workspace.SetupTimeoutSeconds; s > 0 {
		timeout = time.Duration(s) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ws := r.start.Workspace
	if len(ws.Skills) > 0 {
		if err := r.step(ctx, "skills", func() (string, error) { return "", r.writeSkills(ws.Skills) }); err != nil {
			return err
		}
	}
	for _, repo := range ws.Repositories {
		repo := repo
		if err := r.step(ctx, "clone:"+repo.Path, func() (string, error) { return r.clone(ctx, repo) }); err != nil {
			return err
		}
	}
	if ws.Snapshot.Restore && r.start.Session > 1 {
		if err := r.step(ctx, "snapshot_restore", func() (string, error) { return r.restoreSnapshot(ctx), nil }); err != nil {
			return err
		}
	}
	if len(r.secretFiles) > 0 {
		if err := r.step(ctx, "secret_files", func() (string, error) { return "", r.writeSecretFiles(r.secretFiles) }); err != nil {
			return err
		}
	}
	if ci := r.start.Cloud; ci != nil {
		if err := r.step(ctx, "cloud_identity", func() (string, error) { return r.setupCloud(ctx, ci) }); err != nil {
			return err
		}
	}
	for i, c := range ws.SetupCommands {
		c := c
		if err := r.step(ctx, "command:"+strconv.Itoa(i+1), func() (string, error) { return r.shell(ctx, c.Command) }); err != nil {
			return err
		}
	}
	return nil
}

// step runs fn between the events setup.step started and ok or failed.
func (r *runner) step(ctx context.Context, name string, fn func() (string, error)) error {
	r.emit(runnerproto.EventSetupStep, "", runnerproto.SetupStepData{Step: name, Status: "started"})
	t0 := r.now()
	out, err := fn()
	data := runnerproto.SetupStepData{Step: name, Status: "ok", DurationMS: r.now().Sub(t0).Milliseconds(), Output: out}
	if err != nil {
		data.Status = "failed"
		if data.Output == "" {
			data.Output = err.Error()
		}
	}
	r.emit(runnerproto.EventSetupStep, "", data)
	if err == nil {
		return nil
	}
	detail := failureOutput(data.Output, err)
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &setupError{fmt.Sprintf("step %s: setup timed out", name) + detail}
	}
	return &setupError{fmt.Sprintf("step %s failed: %v", name, err) + detail}
}

// failureOutput returns the last maxFailureOutput bytes of the output of a
// failed step, after a line break. It is empty when the output only repeats
// the error. The end message gets it, so that the run says why setup failed.
func failureOutput(out string, err error) string {
	out = strings.TrimSpace(out)
	if out == "" || out == err.Error() {
		return ""
	}
	if len(out) > maxFailureOutput {
		out = "..." + strings.ToValidUTF8(out[len(out)-maxFailureOutput:], "")
	}
	return "\n" + out
}

// writeSkills writes the files of each skill below .openfactory/skills.
func (r *runner) writeSkills(skills []runnerproto.Skill) error {
	root := filepath.Join(r.cfg.WorkDir, ".openfactory", "skills")
	for _, s := range skills {
		if err := checkRelPath(s.Name); err != nil {
			return fmt.Errorf("skill name: %w", err)
		}
		for name, content := range s.Files {
			if err := checkRelPath(name); err != nil {
				return fmt.Errorf("skill %s: file name: %w", s.Name, err)
			}
			p := filepath.Join(root, filepath.FromSlash(s.Name), filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// writeSecretFiles writes each secret file below the workspace. The default
// mode is 0600. The error text never has file content.
func (r *runner) writeSecretFiles(files []runnerproto.SecretFile) error {
	for _, f := range files {
		if err := checkRelPath(f.Path); err != nil {
			return fmt.Errorf("secret file: %w", err)
		}
		mode := os.FileMode(0o600)
		if f.Mode != "" {
			m, err := strconv.ParseUint(f.Mode, 8, 32)
			if err != nil {
				return fmt.Errorf("secret file %s: mode is not an octal number", f.Path)
			}
			mode = os.FileMode(m) & os.ModePerm
		}
		data, err := base64.StdEncoding.DecodeString(f.ContentBase64)
		if err != nil {
			return fmt.Errorf("secret file %s: content is not valid base64", f.Path)
		}
		p := filepath.Join(r.cfg.WorkDir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, data, mode); err != nil {
			return err
		}
		// The umask can remove bits at creation, and an old file keeps its mode.
		if err := os.Chmod(p, mode); err != nil {
			return err
		}
	}
	return nil
}

// clone clones one repository without credentials and checks out its ref.
func (r *runner) clone(ctx context.Context, repo runnerproto.Repository) (string, error) {
	if err := checkRelPath(repo.Path); err != nil {
		return "", fmt.Errorf("repository path: %w", err)
	}
	if repo.CloneURL == "" || strings.HasPrefix(repo.CloneURL, "-") {
		return "", errors.New("clone_url is not valid")
	}
	if strings.HasPrefix(repo.Ref, "-") {
		return "", errors.New("ref is not valid")
	}
	dest := filepath.Join(r.cfg.WorkDir, filepath.FromSlash(repo.Path))
	out, err := r.git(ctx, r.cfg.WorkDir, cloneArgs(r.cfg.GitCredentialHelper, repo.CloneURL, dest)...)
	if err != nil {
		return out, err
	}
	if err := r.configureCredentials(ctx, dest); err != nil {
		return out, err
	}
	if repo.Ref != "" {
		o, err := r.git(ctx, r.cfg.WorkDir, "-C", dest, "checkout", repo.Ref, "--")
		out += o
		if err != nil {
			return tail(out), err
		}
	}
	r.excludeOpenfactory(dest)
	base, err := r.git(ctx, dest, "rev-parse", "HEAD")
	if err != nil {
		return tail(out + base), err
	}
	r.repos = append(r.repos, repoState{path: repo.Path, dir: dest, base: strings.TrimSpace(base)})
	return out, nil
}

// cloneArgs makes the arguments of git for a clone. With a helper, git gets
// the helper for the clone itself.
func cloneArgs(helper, url, dest string) []string {
	var args []string
	if helper != "" {
		args = append(args, "-c", "credential.helper="+helper, "-c", "credential.useHttpPath=true")
	}
	return append(args, "clone", "--", url, dest)
}

// configureCredentials sets the credential helper in the config of a clone.
// The helper gets its credential from the control plane, so the config has
// no credential value.
func (r *runner) configureCredentials(ctx context.Context, dir string) error {
	h := r.cfg.GitCredentialHelper
	if h == "" {
		return nil
	}
	for _, kv := range [][2]string{{"credential.helper", h}, {"credential.useHttpPath", "true"}} {
		if out, err := r.git(ctx, dir, "config", kv[0], kv[1]); err != nil {
			return fmt.Errorf("set %s: %w: %s", kv[0], err, strings.TrimSpace(out))
		}
	}
	return nil
}

// workDirOfTurns returns the work directory of the turns: WorkDir, or the
// directory that OF_WORKDIR names below it.
func (r *runner) workDirOfTurns() string {
	rel := r.start.Env["OF_WORKDIR"]
	if rel == "" {
		return r.cfg.WorkDir
	}
	if err := checkRelPath(rel); err != nil {
		r.diagnostic("bad_workdir", "OF_WORKDIR is not valid, the turns use the workspace: "+err.Error())
		return r.cfg.WorkDir
	}
	return filepath.Join(r.cfg.WorkDir, filepath.FromSlash(rel))
}

// excludeOpenfactory adds .openfactory/ to the local git exclude file.
func (r *runner) excludeOpenfactory(repoDir string) {
	p := filepath.Join(repoDir, ".git", "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString("\n.openfactory/\n")
}

func (r *runner) git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(r.env(), "GIT_TERMINAL_PROMPT=0")
	return runCommand(cmd, dir)
}

// shell runs one setup command in a new shell in the workspace.
func (r *runner) shell(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Env = r.env()
	return runCommand(cmd, r.cfg.WorkDir)
}

// runCommand runs cmd in dir and returns the last 16 KiB of its output.
func runCommand(cmd *exec.Cmd, dir string) (string, error) {
	buf := &tailBuffer{max: maxStepOutput}
	cmd.Dir = dir
	cmd.Stdout = buf
	cmd.Stderr = buf
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	cmd.WaitDelay = killWaitDelay
	err := cmd.Run()
	return buf.String(), err
}

func tail(s string) string {
	if len(s) > maxStepOutput {
		return strings.ToValidUTF8(s[len(s)-maxStepOutput:], "")
	}
	return s
}

// env returns the environment of each process of the runner: the process
// environment without the bootstrap token, then the values of the control
// plane, then the secrets.
func (r *runner) env() []string {
	skip := core.TaskEnvBootstrapToken + "="
	var out []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, skip) {
			out = append(out, e)
		}
	}
	for k, v := range r.start.Env {
		if k != core.TaskEnvBootstrapToken {
			out = append(out, k+"="+v)
		}
	}
	for k, v := range r.cloudEnv {
		out = append(out, k+"="+v)
	}
	for k, v := range r.secretEnv {
		out = append(out, k+"="+v)
	}
	// The agent commits too. Without an identity, git refuses to commit.
	have := map[string]bool{}
	for _, e := range out {
		if k, _, ok := strings.Cut(e, "="); ok {
			have[k] = true
		}
	}
	for _, kv := range gitIdentityEnv() {
		if k, _, _ := strings.Cut(kv, "="); !have[k] {
			out = append(out, kv)
		}
	}
	return out
}
