package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Category names a class of run data. Each category can live in its own
// store.
type Category string

// Run data categories.
const (
	Transcripts Category = "transcripts"
	Artifacts   Category = "artifacts"
	Attachments Category = "attachments"
	Snapshots   Category = "snapshots"
)

// RunData stores per-run objects under this key layout:
//
//	runs/{runID}/harness/{session}.jsonl  raw harness transcripts
//	runs/{runID}/artifacts/{name}
//	runs/{runID}/attachments/{name}
//	snapshots/{team}/{runID}/{session}/{part}/{file}
//
// Snapshot keys are written by the runner API, not by RunData. Transcripts,
// artifacts, attachments and snapshots each use their own Store, so a
// deployment can map them independently (for example artifacts in a
// customer bucket, transcripts on local disk). A store is never cached: every
// call goes to the backend.
type RunData struct {
	transcripts Store
	artifacts   Store
	attachments Store
	snapshots   Store
	now         func() time.Time
}

// NewRunData returns a RunData. A nil category store falls back to def; def
// may be nil only if all four category stores are set.
func NewRunData(def, transcripts, artifacts, attachments, snapshots Store) (*RunData, error) {
	pick := func(s Store) Store {
		if s != nil {
			return s
		}
		return def
	}
	rd := &RunData{transcripts: pick(transcripts), artifacts: pick(artifacts), attachments: pick(attachments), snapshots: pick(snapshots), now: time.Now}
	if rd.transcripts == nil || rd.artifacts == nil || rd.attachments == nil || rd.snapshots == nil {
		return nil, errors.New("objectstore: RunData needs a default store or a store for every category")
	}
	return rd, nil
}

// SetClock overrides the clock used for transcript sequence numbers and
// retention (tests).
func (d *RunData) SetClock(now func() time.Time) { d.now = now }

// Store returns the store used for a category.
func (d *RunData) Store(c Category) Store {
	switch c {
	case Transcripts:
		return d.transcripts
	case Artifacts:
		return d.artifacts
	case Attachments:
		return d.attachments
	case Snapshots:
		return d.snapshots
	}
	return nil
}

func checkRunID(runID string) error {
	if runID == "" || strings.ContainsAny(runID, "/\\") || ValidateKey(runID) != nil {
		return fmt.Errorf("%w: bad run id %q", ErrInvalidKey, runID)
	}
	return nil
}

func runPrefix(runID string) string { return "runs/" + runID + "/" }

func namedKey(runID, dir, name string) (string, error) {
	if err := checkRunID(runID); err != nil {
		return "", err
	}
	if err := ValidateKey(name); err != nil {
		return "", err
	}
	return runPrefix(runID) + dir + "/" + name, nil
}

// --- transcripts ---

// PutHarnessTranscript stores the raw transcript of a harness session under
// runs/{runID}/harness/{session}.jsonl in the transcripts store. It is not part
// of the run message transcript: the format is the one of the harness. A
// later upload of the same session replaces it.
func (d *RunData) PutHarnessTranscript(ctx context.Context, runID string, session int, r io.Reader, size int64) (string, error) {
	if err := checkRunID(runID); err != nil {
		return "", err
	}
	key := fmt.Sprintf("%sharness/%d.jsonl", runPrefix(runID), session)
	return key, d.transcripts.Put(ctx, key, r, size, "application/x-ndjson")
}

// --- artifacts and attachments ---

func (d *RunData) put(ctx context.Context, s Store, runID, dir, name string, r io.Reader, size int64, ct string) (string, error) {
	key, err := namedKey(runID, dir, name)
	if err != nil {
		return "", err
	}
	return key, s.Put(ctx, key, r, size, ct)
}

func (d *RunData) list(ctx context.Context, s Store, runID, dir string) ([]ObjectInfo, error) {
	if err := checkRunID(runID); err != nil {
		return nil, err
	}
	p := runPrefix(runID) + dir + "/"
	objs, err := s.List(ctx, p)
	if err != nil {
		return nil, err
	}
	for i := range objs {
		objs[i].Key = strings.TrimPrefix(objs[i].Key, p)
	}
	return objs, nil
}

// PutArtifact stores an artifact and returns its object key.
func (d *RunData) PutArtifact(ctx context.Context, runID, name string, r io.Reader, size int64, contentType string) (string, error) {
	return d.put(ctx, d.artifacts, runID, "artifacts", name, r, size, contentType)
}

// OpenArtifact reads an artifact.
func (d *RunData) OpenArtifact(ctx context.Context, runID, name string) (io.ReadCloser, ObjectInfo, error) {
	key, err := namedKey(runID, "artifacts", name)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	return d.artifacts.Get(ctx, key)
}

// ListArtifacts lists a run's artifacts; ObjectInfo.Key is the artifact name.
func (d *RunData) ListArtifacts(ctx context.Context, runID string) ([]ObjectInfo, error) {
	return d.list(ctx, d.artifacts, runID, "artifacts")
}

// ArtifactURL returns a presigned download URL for an artifact.
func (d *RunData) ArtifactURL(ctx context.Context, runID, name string, ttl time.Duration) (string, error) {
	key, err := namedKey(runID, "artifacts", name)
	if err != nil {
		return "", err
	}
	return d.artifacts.PresignGet(ctx, key, ttl)
}

// PutAttachment stores a prompt attachment and returns its object key.
func (d *RunData) PutAttachment(ctx context.Context, runID, name string, r io.Reader, size int64, contentType string) (string, error) {
	return d.put(ctx, d.attachments, runID, "attachments", name, r, size, contentType)
}

// OpenAttachment reads a prompt attachment.
func (d *RunData) OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, ObjectInfo, error) {
	key, err := namedKey(runID, "attachments", name)
	if err != nil {
		return nil, ObjectInfo{}, err
	}
	return d.attachments.Get(ctx, key)
}

// ListAttachments lists a run's attachments; ObjectInfo.Key is the name.
func (d *RunData) ListAttachments(ctx context.Context, runID string) ([]ObjectInfo, error) {
	return d.list(ctx, d.attachments, runID, "attachments")
}

// AttachmentURL returns a presigned download URL for an attachment.
func (d *RunData) AttachmentURL(ctx context.Context, runID, name string, ttl time.Duration) (string, error) {
	key, err := namedKey(runID, "attachments", name)
	if err != nil {
		return "", err
	}
	return d.attachments.PresignGet(ctx, key, ttl)
}

// AttachmentUploadURL returns a presigned upload URL so a client can send an
// attachment straight to storage. Send the headers from
// PutHeaders(d.Store(Attachments), contentType) with the PUT.
func (d *RunData) AttachmentUploadURL(ctx context.Context, runID, name string, ttl time.Duration, contentType string) (string, error) {
	key, err := namedKey(runID, "attachments", name)
	if err != nil {
		return "", err
	}
	return d.attachments.PresignPut(ctx, key, ttl, contentType)
}

// --- deletion and retention ---

// distinctStores returns each configured store once.
func (d *RunData) distinctStores() []Store {
	var out []Store
	for _, s := range []Store{d.transcripts, d.artifacts, d.attachments, d.snapshots} {
		dup := false
		for _, o := range out {
			if o == s {
				dup = true
			}
		}
		if !dup {
			out = append(out, s)
		}
	}
	return out
}

const snapshotsPrefix = "snapshots/"

// snapshotRunID returns the run ID of a snapshot key
// snapshots/{team}/{run}/...
func snapshotRunID(key string) (string, bool) {
	parts := strings.SplitN(strings.TrimPrefix(key, snapshotsPrefix), "/", 3)
	if len(parts) < 3 || parts[1] == "" {
		return "", false
	}
	return parts[1], true
}

// DeleteRun removes every object of a run from every category store.
func (d *RunData) DeleteRun(ctx context.Context, runID string) error {
	if err := checkRunID(runID); err != nil {
		return err
	}
	var errs []error
	for _, s := range d.distinctStores() {
		objs, err := s.List(ctx, runPrefix(runID))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		snaps, err := s.List(ctx, snapshotsPrefix)
		if err != nil {
			errs = append(errs, err)
		}
		for _, o := range snaps {
			if id, ok := snapshotRunID(o.Key); ok && id == runID {
				objs = append(objs, o)
			}
		}
		for _, o := range objs {
			if err := s.Delete(ctx, o.Key); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Prune deletes every run whose newest object (across all categories) is
// older than olderThan, and returns the IDs of the runs it deleted. It is
// safe to run on several replicas at once; deletes are idempotent.
func (d *RunData) Prune(ctx context.Context, olderThan time.Duration) ([]string, error) {
	if olderThan <= 0 {
		return nil, errors.New("objectstore: prune age must be positive")
	}
	cutoff := d.now().Add(-olderThan)
	type runObjs struct {
		newest time.Time
		objs   map[Store][]string
	}
	runs := map[string]*runObjs{}
	stores := d.distinctStores()
	for _, s := range stores {
		objs, err := s.List(ctx, "runs/")
		if err != nil {
			return nil, err
		}
		snaps, err := s.List(ctx, snapshotsPrefix)
		if err != nil {
			return nil, err
		}
		objs = append(objs, snaps...)
		for _, o := range objs {
			var id string
			var ok bool
			if strings.HasPrefix(o.Key, snapshotsPrefix) {
				id, ok = snapshotRunID(o.Key)
			} else {
				id, _, ok = strings.Cut(strings.TrimPrefix(o.Key, "runs/"), "/")
			}
			if !ok || id == "" {
				continue
			}
			r := runs[id]
			if r == nil {
				r = &runObjs{objs: map[Store][]string{}}
				runs[id] = r
			}
			if o.LastModified.After(r.newest) {
				r.newest = o.LastModified
			}
			r.objs[s] = append(r.objs[s], o.Key)
		}
	}
	var pruned []string
	var errs []error
	ids := make([]string, 0, len(runs))
	for id := range runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		r := runs[id]
		if r.newest.IsZero() || !r.newest.Before(cutoff) {
			continue
		}
		failed := false
		for _, s := range stores {
			for _, k := range r.objs[s] {
				if err := s.Delete(ctx, k); err != nil {
					errs = append(errs, err)
					failed = true
				}
			}
		}
		if !failed {
			pruned = append(pruned, id)
		}
	}
	return pruned, errors.Join(errs...)
}

// uploadedAttachmentsPrefix is the key prefix of the files that a person
// uploads with POST /attachments (attachments/{team}/{id}/{name}). The API
// writes them to the artifacts store.
const uploadedAttachmentsPrefix = "attachments/"

// PruneAttachments deletes the uploaded attachments (the files of
// POST /attachments) that are older than olderThan, and returns the IDs of
// the attachments that it deleted. Prune does not cover them: the key of an
// uploaded file has a team and an attachment ID, and no run ID.
//
// Only the age of the file decides. The data model does not link an
// attachment to a run: the ID is in the data of the messages that use it, and
// a file can be uploaded and not used. The age of the file is the age of the
// upload, and so it is also the oldest age that a message with that file can
// have. It is safe to run on several replicas at once; deletes are
// idempotent.
func (d *RunData) PruneAttachments(ctx context.Context, olderThan time.Duration) ([]string, error) {
	if olderThan <= 0 {
		return nil, errors.New("objectstore: prune age must be positive")
	}
	cutoff := d.now().Add(-olderThan)
	objs, err := d.artifacts.List(ctx, uploadedAttachmentsPrefix)
	if err != nil {
		return nil, err
	}
	var errs []error
	failed := map[string]bool{}
	deleted := map[string]bool{}
	for _, o := range objs {
		// attachments/{team}/{id}/{name}
		parts := strings.SplitN(strings.TrimPrefix(o.Key, uploadedAttachmentsPrefix), "/", 3)
		if len(parts) < 3 || parts[1] == "" || o.LastModified.IsZero() || !o.LastModified.Before(cutoff) {
			continue
		}
		if err := d.artifacts.Delete(ctx, o.Key); err != nil {
			errs = append(errs, err)
			failed[parts[1]] = true
			continue
		}
		deleted[parts[1]] = true
	}
	ids := make([]string, 0, len(deleted))
	for id := range deleted {
		if !failed[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, errors.Join(errs...)
}
