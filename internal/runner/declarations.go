package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/snapshot"
)

const declarationsTimeout = 30 * time.Second

type declaration struct {
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// readDeclarations reads the declarations of OF_SNAPSHOT_DECLARATIONS. If the
// file is executable, it runs the file and reads its output. Paths are as
// declared: absolute, or relative to the workspace.
func (r *runner) readDeclarations(ctx context.Context) (repos []string, files []string) {
	path := r.cfg.SnapshotDeclarations
	if path == "" {
		return nil, nil
	}
	data, err := r.declarationData(ctx, path)
	if err != nil {
		r.diagnostic("snapshot/bad_declaration", "declarations: "+scrub(err))
		return nil, nil
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var d declaration
		switch err := json.Unmarshal([]byte(line), &d); {
		case err != nil:
			r.diagnostic("snapshot/bad_declaration", "line "+strconv.Itoa(n)+": not valid JSON")
		case d.Path == "":
			r.diagnostic("snapshot/bad_declaration", "line "+strconv.Itoa(n)+": path is empty")
		case d.Kind == "repo":
			repos = append(repos, d.Path)
		case d.Kind == "file":
			files = append(files, d.Path)
		default:
			r.diagnostic("snapshot/bad_declaration", fmt.Sprintf("line %d: unknown kind %q", n, d.Kind))
		}
	}
	if err := sc.Err(); err != nil {
		r.diagnostic("snapshot/bad_declaration", "declarations: "+err.Error())
	}
	return repos, files
}

func (r *runner) declarationData(ctx context.Context, path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o111 == 0 {
		return os.ReadFile(path)
	}
	ctx, cancel := context.WithTimeout(ctx, declarationsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Dir = r.cfg.WorkDir
	skip := rtcore.TaskEnvBootstrapToken + "="
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, skip) {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.WaitDelay = time.Second
	r.asAgent(cmd)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", path, err)
	}
	return out, nil
}

// absWork makes p absolute; a relative p is relative to the workspace.
func (r *runner) absWork(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(r.cfg.WorkDir, p)
}

// manifestPath is the path of a repository in the manifest: relative to the
// workspace if it is inside, else absolute.
func (r *runner) manifestPath(abs string) string {
	rel, err := filepath.Rel(r.cfg.WorkDir, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs
	}
	return filepath.ToSlash(rel)
}

// captureDeclaredRepos saves each declared repository that is not one of the
// configured repositories.
func (r *runner) captureDeclaredRepos(ctx context.Context, dir string, decl []string, m *snapshot.Manifest) {
	seen := map[string]bool{}
	for _, st := range r.repos {
		seen[filepath.Clean(st.dir)] = true
	}
	idx := len(r.repos)
	for _, p := range decl {
		abs := r.absWork(p)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		mp := r.manifestPath(abs)
		e, skipped, err := snapshot.CaptureRepo(r.withGitHook(ctx), snapshot.CaptureOptions{
			RepoDir: abs, Path: mp, OutDir: dir, Index: idx,
		})
		idx++
		if err != nil {
			r.diagnostic("snapshot_failed", "declared repository "+mp+": "+err.Error())
			m.Skipped = append(m.Skipped, snapshot.Skipped{Path: mp, Reason: "capture_failed"})
			continue
		}
		m.Repositories = append(m.Repositories, e)
		m.Skipped = append(m.Skipped, skipped...)
	}
}

// captureDeclaredFiles copies each declared file into the part as files/<index>.
func (r *runner) captureDeclaredFiles(dir string, decl []string, m *snapshot.Manifest) {
	seen := map[string]bool{}
	for _, p := range decl {
		abs := r.absWork(p)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		info, err := os.Stat(abs)
		switch {
		case os.IsNotExist(err):
			m.Skipped = append(m.Skipped, snapshot.Skipped{Path: p, Reason: "missing"})
			continue
		case err != nil:
			r.diagnostic("snapshot_failed", "declared file "+p+": "+scrub(err))
			m.Skipped = append(m.Skipped, snapshot.Skipped{Path: p, Reason: "capture_failed"})
			continue
		case !info.Mode().IsRegular():
			m.Skipped = append(m.Skipped, snapshot.Skipped{Path: p, Reason: "not_regular"})
			continue
		}
		obj := "files/" + strconv.Itoa(len(m.Files))
		if err := copyFile(abs, filepath.Join(dir, filepath.FromSlash(obj)), 0o600); err != nil {
			r.diagnostic("snapshot_failed", "declared file "+p+": "+scrub(err))
			m.Skipped = append(m.Skipped, snapshot.Skipped{Path: p, Reason: "capture_failed"})
			continue
		}
		m.Files = append(m.Files, snapshot.FileEntry{Path: p, Object: obj, Mode: fmt.Sprintf("%04o", info.Mode().Perm())})
	}
}

// restoreFiles writes the files of the manifest. It returns the paths that failed.
func (r *runner) restoreFiles(m snapshot.Manifest, dir string) []string {
	var failed []string
	for _, f := range m.Files {
		mode := os.FileMode(0o600)
		if v, err := strconv.ParseUint(f.Mode, 8, 32); err == nil {
			mode = os.FileMode(v).Perm()
		}
		dst := r.absWork(f.Path)
		if err := copyFile(filepath.Join(dir, filepath.FromSlash(f.Object)), dst, mode); err != nil {
			r.diagnostic("snapshot_restore_failed", "file "+f.Path+": "+err.Error())
			failed = append(failed, f.Path)
		}
	}
	return failed
}

// copyFile copies src to dst with the permissions mode. It creates the
// parent directories.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

// restoreDeclaredRepo restores a repository that is not a configured
// repository into its path. If the directory is not a clone yet and the
// entry has a remote, it clones the remote first.
func (r *runner) restoreDeclaredRepo(ctx context.Context, e snapshot.RepoEntry, partDir string) bool {
	dir := r.absWork(e.Path)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if e.Remote == "" {
			r.diagnostic("snapshot_restore_failed", "repository "+e.Path+" does not exist and has no remote")
			return false
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			r.diagnostic("snapshot_restore_failed", "repository "+e.Path+": "+err.Error())
			return false
		}
		if out, err := r.git(ctx, r.cfg.WorkDir, "clone", "-q", "--no-checkout", e.Remote, dir); err != nil {
			r.diagnostic("snapshot_restore_failed", "repository "+e.Path+": clone: "+err.Error()+": "+strings.TrimSpace(tail(out)))
			return false
		}
	}
	if err := r.checkoutBase(ctx, dir, e.UpstreamSHA); err != nil {
		r.diagnostic("snapshot_restore_failed", "repository "+e.Path+": "+err.Error())
		return false
	}
	if err := snapshot.RestoreRepo(r.withGitHook(ctx), dir, e, partDir); err != nil {
		r.diagnostic("snapshot_restore_failed", "repository "+e.Path+": "+err.Error())
		return false
	}
	return true
}
