package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/snapshot"
)

const (
	snapshotTimeout = 5 * time.Minute
	noteRestored    = "The workspace was restored from the previous session."
	noteNew         = "No snapshot of the workspace exists. The workspace is new."
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// repoState is one cloned repository of the workspace.
type repoState struct {
	path string // path in the workspace, from the start response
	dir  string // absolute directory of the clone
	base string // newest commit that the remote has; the base of a capture
}

// gitIdentityEnv gives git a fixed identity if the environment has none.
// Without an identity, git can wait for a host name lookup.
func gitIdentityEnv() []string {
	var env []string
	for _, kv := range [][2]string{
		{"GIT_AUTHOR_NAME", "openfactory"},
		{"GIT_AUTHOR_EMAIL", "openfactory@localhost"},
		{"GIT_COMMITTER_NAME", "openfactory"},
		{"GIT_COMMITTER_EMAIL", "openfactory@localhost"},
	} {
		if os.Getenv(kv[0]) == "" {
			env = append(env, kv[0]+"="+kv[1])
		}
	}
	return env
}

// snapshotReason says whether a session that ends for this reason takes a snapshot.
func snapshotReason(reason runnerproto.EndReason) bool {
	switch reason {
	case runnerproto.EndIdle, runnerproto.EndStopped, runnerproto.EndCancelled, runnerproto.EndDeadline:
		return true
	}
	return false
}

// withRestoreNote puts the restore note in front of the input of the first
// turn. The note is used once.
func (r *runner) withRestoreNote(input []runnerproto.InputItem) []runnerproto.InputItem {
	if r.restoreNote == "" {
		return input
	}
	note := runnerproto.InputItem{
		MessageID:   "restore_note",
		Kind:        runnerproto.KindSystem,
		Sender:      runnerproto.Sender{Type: "platform"},
		Content:     r.restoreNote,
		Attachments: []runnerproto.Attachment{},
	}
	r.restoreNote = ""
	return append([]runnerproto.InputItem{note}, input...)
}

func (r *runner) httpClient() *http.Client {
	if r.cfg.HTTPClient != nil {
		return r.cfg.HTTPClient
	}
	return &http.Client{}
}

// scrub returns the text of err without any URL of a presigned request.
func scrub(err error) string {
	msg := err.Error()
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, ue.URL, "<url>")
	}
	return msg
}

// restoreSnapshot restores the workspace part and the harness_state part and
// sends snapshot.restored. It never fails: a problem adds a diagnostic or a
// failed path.
func (r *runner) restoreSnapshot(ctx context.Context) string {
	parts, failed := []string{}, []string{}
	var byPart map[string][]runnerproto.DownloadFile
	resp, err := r.client.Downloads(ctx, string(runnerproto.UploadSnapshot), "")
	if err != nil {
		r.diagnostic("snapshot_restore_failed", "list snapshot files: "+scrub(err))
	} else {
		byPart = map[string][]runnerproto.DownloadFile{}
		for _, f := range resp.Files {
			byPart[f.Part] = append(byPart[f.Part], f)
		}
	}
	if m, dir, ok := r.loadPart(ctx, snapshot.PartWorkspace, byPart[snapshot.PartWorkspace]); ok {
		parts = append(parts, snapshot.PartWorkspace)
		failed = r.restoreRepos(ctx, m, dir)
		failed = append(failed, r.restoreFiles(m, dir)...)
	}
	if files := byPart[snapshot.PartHarnessState]; len(files) > 0 {
		if r.restoreHarnessState(ctx, files) {
			parts = append(parts, snapshot.PartHarnessState)
		} else {
			failed = append(failed, snapshot.PartHarnessState)
		}
	}
	r.emit(runnerproto.EventSnapshotRestored, "", runnerproto.SnapshotRestoredData{Parts: parts, FailedPaths: failed})
	found := false
	for _, p := range parts {
		found = found || p == snapshot.PartWorkspace
	}
	r.restoreNote = restoreNote(found, failed)
	return fmt.Sprintf("parts: %d, failed paths: %d", len(parts), len(failed))
}

func restoreNote(found bool, failed []string) string {
	switch {
	case !found:
		return noteNew
	case len(failed) > 0:
		return noteRestored + " These paths could not be restored: " + strings.Join(failed, ", ") + "."
	}
	return noteRestored
}

// loadPart downloads one part and reads its manifest. It returns false if
// the part has no manifest file or is not complete.
func (r *runner) loadPart(ctx context.Context, part string, files []runnerproto.DownloadFile) (snapshot.Manifest, string, bool) {
	var none snapshot.Manifest
	found := false
	for _, f := range files {
		found = found || f.Name == snapshot.ManifestName
	}
	if !found {
		return none, "", false
	}
	dir := filepath.Join(r.cfg.StateDir, "restore", part)
	if err := r.fetchPart(ctx, dir, files); err != nil {
		r.diagnostic("snapshot_restore_failed", "download snapshot: "+scrub(err))
		return none, "", false
	}
	m, err := snapshot.ReadManifest(dir)
	if err == nil && m.Part != part {
		err = fmt.Errorf("the manifest is for part %q, not %q", m.Part, part)
	}
	if err != nil || !m.Complete {
		if err == nil {
			err = errors.New("the snapshot is not complete")
		}
		r.diagnostic("snapshot_restore_failed", "read snapshot manifest: "+err.Error())
		return none, "", false
	}
	return m, dir, true
}

// fetchPart downloads the files into dir. It removes the old content first.
func (r *runner) fetchPart(ctx context.Context, dir string, files []runnerproto.DownloadFile) error {
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	for _, f := range files {
		if err := checkRelPath(f.Name); err != nil {
			return fmt.Errorf("file name: %w", err)
		}
		if err := r.fetchFile(ctx, filepath.Join(dir, filepath.FromSlash(f.Name)), f); err != nil {
			return fmt.Errorf("file %s: %w", f.Name, err)
		}
	}
	return nil
}

// fetchFile gets one presigned URL, without the session token.
func (r *runner) fetchFile(ctx context.Context, path string, f runnerproto.DownloadFile) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.URL, nil)
	if err != nil {
		return err
	}
	resp, err := r.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(resp.Body, f.Bytes+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != f.Bytes {
		return fmt.Errorf("got more or less than %d bytes", f.Bytes)
	}
	return nil
}

// restoreRepos restores each repository of the manifest. It returns the
// paths that failed.
func (r *runner) restoreRepos(ctx context.Context, m snapshot.Manifest, dir string) []string {
	failed := []string{}
	for _, e := range m.Repositories {
		if !r.restoreRepo(ctx, e, dir) {
			failed = append(failed, e.Path)
		}
	}
	return failed
}

func (r *runner) restoreRepo(ctx context.Context, e snapshot.RepoEntry, dir string) bool {
	var st *repoState
	for i := range r.repos {
		if r.repos[i].path == e.Path {
			st = &r.repos[i]
		}
	}
	if st == nil {
		return r.restoreDeclaredRepo(ctx, e, dir)
	}
	if err := r.checkoutBase(ctx, st.dir, e.UpstreamSHA); err != nil {
		r.diagnostic("snapshot_restore_failed", "repository "+e.Path+": "+err.Error())
		return false
	}
	st.base = e.UpstreamSHA
	if err := snapshot.RestoreRepo(ctx, st.dir, e, dir); err != nil {
		r.diagnostic("snapshot_restore_failed", "repository "+e.Path+": "+err.Error())
		return false
	}
	return true
}

// checkoutBase checks out a commit of the remote. If the clone does not have
// the commit, it fetches the commit and tries again.
func (r *runner) checkoutBase(ctx context.Context, dir, sha string) error {
	if !shaPattern.MatchString(sha) {
		return errors.New("upstream_sha is not valid")
	}
	if _, err := r.git(ctx, dir, "checkout", "-q", "--detach", sha); err == nil {
		return nil
	}
	if out, err := r.git(ctx, dir, "fetch", "origin", sha); err != nil {
		return fmt.Errorf("fetch %s: %w: %s", sha, err, strings.TrimSpace(tail(out)))
	}
	if out, err := r.git(ctx, dir, "checkout", "-q", "--detach", sha); err != nil {
		return fmt.Errorf("check out %s: %w: %s", sha, err, strings.TrimSpace(tail(out)))
	}
	return nil
}

// captureSnapshot saves and uploads the workspace part. A failure sends a
// diagnostic and does not stop the end of the session. The time budget is
// separate from the budget of the end calls.
func (r *runner) captureSnapshot() {
	if r.start.Workspace.Snapshot.SkipWorkspace {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	defer cancel()
	ids, total, err := r.saveWorkspace(ctx)
	if errors.Is(err, errPartTooLarge) {
		return // already reported
	}
	if err != nil {
		r.diagnostic("snapshot_failed", scrub(err))
		return
	}
	r.emit(runnerproto.EventSnapshotSaved, "", runnerproto.SnapshotSavedData{
		Part: snapshot.PartWorkspace, UploadIDs: ids, Bytes: total,
	})
}

// saveWorkspace writes the part below StateDir and uploads it.
func (r *runner) saveWorkspace(ctx context.Context) ([]string, int64, error) {
	dir := filepath.Join(r.cfg.StateDir, "snapshot", snapshot.PartWorkspace)
	if err := os.RemoveAll(dir); err != nil {
		return nil, 0, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, 0, err
	}
	m := snapshot.Manifest{
		Version:   snapshot.Version,
		Part:      snapshot.PartWorkspace,
		RunID:     r.start.RunID,
		Session:   r.start.Session,
		CreatedAt: r.now().UTC(),
		Complete:  true,
	}
	for i, st := range r.repos {
		e, skipped, err := snapshot.CaptureRepo(ctx, snapshot.CaptureOptions{
			RepoDir: st.dir, Path: st.path, BaseSHA: st.base, OutDir: dir, Index: i,
		})
		if err != nil {
			r.diagnostic("snapshot_failed", "repository "+st.path+": "+err.Error())
			m.Skipped = append(m.Skipped, snapshot.Skipped{Path: st.path, Reason: "capture_failed"})
			continue
		}
		m.Repositories = append(m.Repositories, e)
		m.Skipped = append(m.Skipped, skipped...)
	}
	declRepos, declFiles := r.readDeclarations(ctx)
	r.captureDeclaredRepos(ctx, dir, declRepos, &m)
	r.captureDeclaredFiles(dir, declFiles, &m)
	if err := snapshot.WriteManifest(dir, m); err != nil {
		return nil, 0, err
	}
	if r.partTooLarge(snapshot.PartWorkspace, dir) {
		return nil, 0, errPartTooLarge
	}
	return r.uploadPart(ctx, snapshot.PartWorkspace, dir)
}

// uploadPart uploads each file of the part directory as part "part". The manifest is last.
func (r *runner) uploadPart(ctx context.Context, part, dir string) ([]string, int64, error) {
	names, err := partFileNames(dir)
	if err != nil {
		return nil, 0, err
	}
	var ids []string
	var total int64
	for _, name := range names {
		id, n, err := r.uploadFile(ctx, part, dir, name)
		if err != nil {
			return nil, 0, fmt.Errorf("upload %s: %w", name, errors.New(scrub(err)))
		}
		ids = append(ids, id)
		total += n
	}
	return ids, total, nil
}

// partFileNames lists the files below dir with forward slashes. The
// manifest is the last name.
func partFileNames(dir string) ([]string, error) {
	var names []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err == nil && rel != snapshot.ManifestName {
			names = append(names, filepath.ToSlash(rel))
		}
		return err
	})
	sort.Strings(names)
	return append(names, snapshot.ManifestName), err
}

func (r *runner) uploadFile(ctx context.Context, part, dir, name string) (string, int64, error) {
	return r.uploadPath(ctx, runnerproto.UploadSnapshot, part, name, filepath.Join(dir, filepath.FromSlash(name)), "application/octet-stream")
}

// uploadPath uploads the file at path as an object of the given kind. part is
// set for a snapshot only. It returns the upload ID and the size.
func (r *runner) uploadPath(ctx context.Context, kind runnerproto.UploadKind, part, name, path, contentType string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	up, err := r.client.RequestUpload(ctx, runnerproto.UploadRequest{
		Kind:        kind,
		Part:        part,
		Name:        name,
		Bytes:       size,
		SHA256:      hex.EncodeToString(h.Sum(nil)),
		ContentType: contentType,
	})
	if err != nil {
		return "", 0, err
	}
	if err := r.client.PutFile(ctx, up, f, size); err != nil {
		return "", 0, err
	}
	return up.UploadID, size, nil
}
