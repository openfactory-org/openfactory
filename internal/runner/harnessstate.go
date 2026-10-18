package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/snapshot"
)

const harnessObject = "harness/state.tar.zst"

// errPartTooLarge says that a part is over the limit. The runner has sent
// the diagnostic already.
var errPartTooLarge = errors.New("snapshot part is too large")

// StateHarness is a Harness that keeps session files. The runner saves
// them in the harness_state part after each turn.
type StateHarness interface {
	// StateFiles returns the session files, relative to home.
	// home is the home directory of the task user. workDir is the work
	// directory of the turns. It returns no paths if the files do not exist.
	StateFiles(home, workDir string) ([]string, error)
}

// noteHarnessEvent keeps the newest harness session ID of the events of a turn.
func (r *runner) noteHarnessEvent(typ runnerproto.EventType, data any) {
	if typ != runnerproto.EventHarnessSession {
		return
	}
	if d, ok := data.(runnerproto.HarnessSessionData); ok && d.SessionID != "" {
		r.sessionMu.Lock()
		r.harnessSessionID = d.SessionID
		r.sessionMu.Unlock()
	}
}

func (r *runner) knownHarnessSession() string {
	r.sessionMu.Lock()
	defer r.sessionMu.Unlock()
	if r.harnessSessionID != "" {
		return r.harnessSessionID
	}
	return r.start.Harness.ResumeSessionID
}

// saveHarnessState uploads the harness_state part. A problem is reported as
// a diagnostic and never fails the turn.
func (r *runner) saveHarnessState(ctx context.Context) {
	if !r.start.Workspace.Snapshot.Enabled || r.cfg.HomeDir == "" {
		return
	}
	sh, ok := r.harness.(StateHarness)
	if !ok {
		return
	}
	if err := r.uploadHarnessState(ctx, sh); err != nil && !errors.Is(err, errPartTooLarge) {
		r.log.Warn("save harness state failed", "error", scrub(err))
		r.diagnostic("snapshot/save_failed", "harness_state: "+scrub(err))
	}
}

func (r *runner) uploadHarnessState(ctx context.Context, sh StateHarness) error {
	paths, err := sh.StateFiles(r.cfg.HomeDir, r.turnDir)
	if err != nil {
		return fmt.Errorf("list session files: %w", err)
	}
	if len(paths) == 0 {
		return nil
	}
	dir := filepath.Join(r.cfg.StateDir, "snapshot-harness")
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "harness"), 0o700); err != nil {
		return err
	}
	if _, err := snapshot.PackFiles(filepath.Join(dir, filepath.FromSlash(harnessObject)), r.cfg.HomeDir, paths); err != nil {
		return err
	}
	if err := snapshot.WriteManifest(dir, snapshot.Manifest{
		Version:   snapshot.Version,
		Part:      snapshot.PartHarnessState,
		RunID:     r.start.RunID,
		Session:   r.start.Session,
		CreatedAt: r.now().UTC(),
		Complete:  true,
		Harness: &snapshot.HarnessEntry{
			Type:      r.start.Harness.Type,
			SessionID: r.knownHarnessSession(),
			Object:    harnessObject,
		},
	}); err != nil {
		return err
	}
	if r.partTooLarge(snapshot.PartHarnessState, dir) {
		return errPartTooLarge
	}
	ids, total, err := r.uploadPart(ctx, snapshot.PartHarnessState, dir)
	if err != nil {
		return err
	}
	r.emit(runnerproto.EventSnapshotSaved, "", runnerproto.SnapshotSavedData{
		Part: snapshot.PartHarnessState, UploadIDs: ids, Bytes: total,
	})
	return nil
}

// partTooLarge sends the diagnostic snapshot/too_large and returns true if
// the files of the part directory are more than MaxPartBytes.
func (r *runner) partTooLarge(part, dir string) bool {
	limit := r.start.Workspace.Snapshot.MaxPartBytes
	if limit <= 0 {
		return false
	}
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, ierr := d.Info(); ierr == nil {
				total += info.Size()
			}
		}
		return nil
	})
	if total <= limit {
		return false
	}
	r.diagnostic("snapshot/too_large", fmt.Sprintf("the %s part is %d bytes, more than the limit of %d bytes; it was not uploaded", part, total, limit))
	return true
}

// restoreHarnessState downloads the harness_state part and unpacks it into
// the home directory. It returns true on success.
func (r *runner) restoreHarnessState(ctx context.Context, files []runnerproto.DownloadFile) bool {
	if r.cfg.HomeDir == "" {
		r.diagnostic("snapshot_restore_failed", "harness_state: the home directory is not known")
		return false
	}
	m, dir, ok := r.loadPart(ctx, snapshot.PartHarnessState, files)
	if !ok {
		return false
	}
	if err := checkRelPath(m.Harness.Object); err != nil {
		r.diagnostic("snapshot_restore_failed", "harness_state: "+err.Error())
		return false
	}
	if err := snapshot.Unpack(filepath.Join(dir, filepath.FromSlash(m.Harness.Object)), r.cfg.HomeDir); err != nil {
		r.diagnostic("snapshot_restore_failed", "harness_state: "+err.Error())
		return false
	}
	r.harnessStateRestored = true
	return true
}
