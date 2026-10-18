package runner

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	uploadTimeout = 5 * time.Minute
	// Limits of the artifacts of one session.
	maxArtifactFiles       = 100
	maxArtifactBytes int64 = 100 << 20
	// maxTranscriptBytes is the largest raw transcript that the runner reads into memory.
	maxTranscriptBytes int64 = 200 << 20
)

// TranscriptHarness is an optional interface of a Harness that keeps a raw
// transcript of its session. The runner uploads it after each turn.
type TranscriptHarness interface {
	// TranscriptPath returns the path of the raw transcript file, or "" if
	// there is none. home is the home directory of the task user. workDir is
	// the work directory of the turns.
	TranscriptPath(home, workDir string) string
}

// ArtifactHarness is an optional interface of a Harness that reports artifact files.
type ArtifactHarness interface {
	// TakeDeclaredArtifacts returns the paths that the harness reported since
	// the last call. A path is absolute or relative to the work directory.
	TakeDeclaredArtifacts() []string
}

// fileStamp tells whether a file changed: size and modification time.
type fileStamp struct {
	size  int64
	mtime int64
}

// artifactState is the memory of the artifact upload of one session.
type artifactState struct {
	seen  map[string]fileStamp // name -> stamp of the last upload
	sizes map[string]int64     // name -> size of the last upload, for the byte cap
	total int64
}

// afterTurn uploads the artifacts and the raw transcript of a turn. A problem
// is reported as a diagnostic and never fails the turn.
func (r *runner) afterTurn(turnID string) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	r.uploadArtifacts(ctx, turnID)
	r.uploadTranscript(ctx, turnID)
}

type artifactCandidate struct {
	name, path string
	stamp      fileStamp
}

// artifactCandidates lists the files below the artifacts directory and the
// files that the harness reported, in the order of their names.
func (r *runner) artifactCandidates() []artifactCandidate {
	byName := map[string]artifactCandidate{}
	add := func(name, p string, info fs.FileInfo) {
		if !validArtifactName(name) {
			return
		}
		byName[name] = artifactCandidate{name: name, path: p, stamp: fileStamp{size: info.Size(), mtime: info.ModTime().UnixNano()}}
	}
	dir := filepath.Join(r.turnDir, filepath.FromSlash(core.ArtifactsDir))
	if info, err := os.Lstat(dir); err == nil && info.IsDir() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return nil //nolint:nilerr // best-effort list: an unreadable entry is skipped, a symlink is not followed
			}
			rel, err := filepath.Rel(dir, p)
			if err != nil {
				return nil //nolint:nilerr // best-effort list: a path outside dir is skipped
			}
			if info, err := d.Info(); err == nil {
				add(filepath.ToSlash(rel), p, info)
			}
			return nil
		})
	}
	if ah, ok := r.harness.(ArtifactHarness); ok {
		root, err := filepath.EvalSymlinks(r.turnDir)
		if err != nil {
			root = r.turnDir
		}
		for _, p := range ah.TakeDeclaredArtifacts() {
			if !filepath.IsAbs(p) {
				p = filepath.Join(r.turnDir, p)
			}
			real, err := filepath.EvalSymlinks(p) // the file must be inside the work directory
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(root, real)
			if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
				continue
			}
			if info, err := os.Lstat(real); err == nil && info.Mode().IsRegular() {
				add(filepath.ToSlash(rel), real, info)
			}
		}
	}
	out := make([]artifactCandidate, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// validArtifactName accepts a clean relative path with forward slashes.
func validArtifactName(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\") || path.Clean(name) != name || path.IsAbs(name) {
		return false
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

// uploadArtifacts uploads each artifact file that is new or changed and sends
// an artifact event for it. At most 100 files and 100 MiB are uploaded in a
// session. Over the cap, it sends the diagnostic artifact/limit.
func (r *runner) uploadArtifacts(ctx context.Context, turnID string) {
	if r.arts.seen == nil {
		r.arts.seen, r.arts.sizes = map[string]fileStamp{}, map[string]int64{}
	}
	var skipped []string
	for _, c := range r.artifactCandidates() {
		if old, ok := r.arts.seen[c.name]; ok && old == c.stamp {
			continue
		}
		_, known := r.arts.sizes[c.name]
		if (!known && len(r.arts.sizes) >= maxArtifactFiles) || r.arts.total-r.arts.sizes[c.name]+c.stamp.size > maxArtifactBytes {
			skipped = append(skipped, c.name)
			continue
		}
		if c.stamp.size == 0 {
			continue // the upload API does not take an empty file
		}
		ct := mime.TypeByExtension(path.Ext(c.name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		id, size, err := r.uploadPath(ctx, runnerproto.UploadArtifact, "", c.name, c.path, ct)
		if err != nil {
			r.log.Warn("upload artifact failed", "name", c.name, "error", scrub(err))
			r.diagnostic("artifact/upload_failed", c.name+": "+scrub(err))
			continue
		}
		r.arts.total += size - r.arts.sizes[c.name]
		r.arts.sizes[c.name] = size
		r.arts.seen[c.name] = c.stamp
		r.emit(runnerproto.EventArtifact, turnID, runnerproto.ArtifactData{Name: c.name, Type: "FILE", UploadID: id})
	}
	if len(skipped) > 0 {
		r.diagnostic("artifact/limit", "artifacts over the limit of 100 files or 100 MiB were not uploaded: "+strings.Join(skipped, ", "))
	}
}

// uploadTranscript uploads the raw transcript of the harness after the
// redactor ran over it. It sends transcript.saved when the content changed.
func (r *runner) uploadTranscript(ctx context.Context, turnID string) {
	th, ok := r.harness.(TranscriptHarness)
	if !ok || r.cfg.HomeDir == "" {
		return
	}
	src := th.TranscriptPath(r.cfg.HomeDir, r.turnDir)
	if src == "" {
		return
	}
	fail := func(code, msg string) {
		r.log.Warn("save transcript failed", "error", msg)
		r.diagnostic(code, msg)
	}
	info, err := os.Stat(src)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return
	}
	if info.Size() > maxTranscriptBytes {
		fail("transcript/too_large", "the raw transcript is over the limit and was not uploaded")
		return
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		fail("transcript/read_failed", err.Error())
		return
	}
	clean := []byte(redactString(string(raw), redactTerms(r.redact)))
	sum := sha256.Sum256(clean)
	if sum == r.transcriptSum {
		return
	}
	tmp, err := os.CreateTemp(r.cfg.StateDir, "transcript-*.jsonl")
	if err != nil {
		fail("transcript/write_failed", err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(clean)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		fail("transcript/write_failed", werr.Error())
		return
	}
	id, _, err := r.uploadPath(ctx, runnerproto.UploadTranscript, "", "transcript.jsonl", tmp.Name(), "application/x-ndjson")
	if err != nil {
		fail("transcript/upload_failed", scrub(err))
		return
	}
	r.transcriptSum = sum
	r.emit(runnerproto.EventTranscriptSaved, turnID, runnerproto.TranscriptSavedData{UploadID: id})
}
