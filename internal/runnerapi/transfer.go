package runnerapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	defaultUploadTTL = 15 * time.Minute

	partWorkspace    = "workspace"
	partHarnessState = "harness_state"

	// defaultTeam is the team name in a snapshot key when the run has no team.
	defaultTeam = "default"
)

// Default size limits per upload kind.
const (
	defaultMaxArtifact   int64 = 100 << 20
	defaultMaxTranscript int64 = 200 << 20
	defaultMaxSnapshot   int64 = 2 << 30
)

// FileStore presigns and checks objects. An objectstore.Store satisfies it.
type FileStore interface {
	PresignPut(ctx context.Context, key string, ttl time.Duration, contentType string) (string, error)
	PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	Stat(ctx context.Context, key string) (objectstore.ObjectInfo, error)
}

func (s *Service) maxUploadFor(k runnerproto.UploadKind) int64 {
	if n, ok := s.maxUpload[k]; ok && n > 0 {
		return n
	}
	switch k {
	case runnerproto.UploadTranscript:
		return defaultMaxTranscript
	case runnerproto.UploadSnapshot:
		return defaultMaxSnapshot
	}
	return defaultMaxArtifact
}

func (s *Service) ttl() time.Duration {
	if s.uploadTTL > 0 {
		return s.uploadTTL
	}
	return defaultUploadTTL
}

func errNotImplemented() *apiError {
	return &apiError{status: http.StatusNotImplemented, code: codeNotImpl, msg: "not implemented"}
}

func teamOf(run *core.Run) string {
	if run.TeamID == "" {
		return defaultTeam
	}
	return run.TeamID
}

// objectPrefix returns the key prefix of an object of a session.
func objectPrefix(run *core.Run, session int, kind runnerproto.UploadKind, part string) string {
	n := strconv.Itoa(session)
	if kind == runnerproto.UploadSnapshot {
		return "snapshots/" + teamOf(run) + "/" + run.ID + "/" + n + "/" + part + "/"
	}
	return "runs/" + run.ID + "/sessions/" + n + "/" + string(kind) + "/"
}

// ownsKey says whether an object key belongs to the run.
func ownsKey(run *core.Run, key string) bool {
	return strings.HasPrefix(key, "runs/"+run.ID+"/") ||
		strings.HasPrefix(key, "snapshots/"+teamOf(run)+"/"+run.ID+"/")
}

func uploadMACWith(k []byte, key string) []byte {
	m := hmac.New(sha256.New, k)
	m.Write([]byte("upload:" + key))
	return m.Sum(nil)
}

// uploadMAC returns the MAC of an object key with the current key.
func (s *Service) uploadMAC(key string) []byte {
	_, k := s.keys.Current()
	return uploadMACWith(k, key)
}

// keyLister is implemented by key sets that can name all their keys.
type keyLister interface{ IDs() []string }

// validUploadMAC says whether mac is the MAC of key under the current key or,
// during a key rotation, under a previous key of the key set.
func (s *Service) validUploadMAC(mac []byte, key string) bool {
	if hmac.Equal(mac, s.uploadMAC(key)) {
		return true
	}
	kl, ok := s.keys.(keyLister)
	if !ok {
		return false
	}
	for _, id := range kl.IDs() {
		if k, found := s.keys.Lookup(id); found && hmac.Equal(mac, uploadMACWith(k, key)) {
			return true
		}
	}
	return false
}

// uploadID makes the upload ID of an object key. The ID holds the key and a
// MAC, so the control plane needs no table of uploads.
func (s *Service) uploadID(key string) string {
	enc := base64.RawURLEncoding
	return "up_" + enc.EncodeToString([]byte(key)) + "." + enc.EncodeToString(s.uploadMAC(key))
}

// parseUploadID returns the key of an upload ID. It checks the MAC.
// It tries the current key and each previous key of the key set.
func (s *Service) parseUploadID(id string) (key string, ok bool) {
	rest, found := strings.CutPrefix(id, "up_")
	if !found {
		return "", false
	}
	k64, m64, found := strings.Cut(rest, ".")
	if !found {
		return "", false
	}
	enc := base64.RawURLEncoding
	kb, err := enc.DecodeString(k64)
	if err != nil {
		return "", false
	}
	mac, err := enc.DecodeString(m64)
	if err != nil {
		return "", false
	}
	key = string(kb)
	if !s.validUploadMAC(mac, key) {
		return "", false
	}
	return key, true
}

func (s *Service) handleUpload(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if s.files == nil && s.snapFiles == nil {
		s.writeError(w, errNotImplemented())
		return
	}
	var req runnerproto.UploadRequest
	if aerr := decodeBody(r, &req); aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if err := req.Validate(); err != nil {
		s.writeError(w, errBad("%v", err))
		return
	}
	if req.Bytes <= 0 {
		s.writeError(w, errBad("bytes must be more than 0"))
		return
	}
	if max := s.maxUploadFor(req.Kind); req.Bytes > max {
		s.writeError(w, &apiError{status: http.StatusRequestEntityTooLarge, code: codeTooLarge,
			msg: fmt.Sprintf("a %s upload is limited to %d bytes", req.Kind, max)})
		return
	}
	if req.Kind == runnerproto.UploadSnapshot && s.snapMax > 0 && req.Bytes > s.snapMax {
		s.writeError(w, &apiError{status: http.StatusRequestEntityTooLarge, code: codeTooLarge,
			msg: fmt.Sprintf("a %s upload is limited to %d bytes", req.Kind, s.snapMax)})
		return
	}
	files, putHeaders := s.files, s.putHeaders
	if req.Kind == runnerproto.UploadSnapshot {
		files, putHeaders = s.snapFiles, s.snapHeaders
	}
	if files == nil {
		s.writeError(w, errNotImplemented())
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil {
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	key := objectPrefix(run, sess.Session, req.Kind, req.Part) + req.Name
	if err := objectstore.ValidateKey(key); err != nil {
		s.writeError(w, errBad("invalid name: %v", err))
		return
	}
	url, err := files.PresignPut(r.Context(), key, s.ttl(), req.ContentType)
	if err != nil {
		s.writeError(w, s.errInternal("presign put", err))
		return
	}
	headers := map[string]string{}
	if putHeaders != nil {
		for k, v := range putHeaders(req.ContentType) {
			headers[k] = v
		}
	} else if req.ContentType != "" {
		headers["Content-Type"] = req.ContentType
	}
	s.writeJSON(w, http.StatusOK, runnerproto.UploadResponse{
		UploadID:  s.uploadID(key),
		Method:    http.MethodPut,
		URL:       url,
		Headers:   headers,
		ExpiresAt: s.now().Add(s.ttl()),
	})
}

func (s *Service) handleDownloads(w http.ResponseWriter, r *http.Request) {
	sess, _, aerr := s.authSession(r)
	if aerr != nil {
		s.writeError(w, aerr)
		return
	}
	if r.URL.Query().Get("kind") == "attachment" {
		s.attachmentDownload(w, r, sess.RunID)
		return
	}
	if s.snapFiles == nil {
		s.writeError(w, errNotImplemented())
		return
	}
	switch kind := r.URL.Query().Get("kind"); kind {
	case string(runnerproto.UploadSnapshot):
	default:
		s.writeError(w, errBad("unknown kind %q", kind))
		return
	}
	run, err := s.store.GetRun(r.Context(), sess.RunID)
	if err != nil {
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	files, err := s.snapshotFiles(r.Context(), run, sess.Session)
	if err != nil {
		s.writeError(w, s.errInternal("snapshot downloads", err))
		return
	}
	s.writeJSON(w, http.StatusOK, runnerproto.DownloadsResponse{Files: files})
}

// snapshotFiles returns the files of the newest snapshot before a session.
// The workspace part and the harness state part can come from different
// sessions.
func (s *Service) snapshotFiles(ctx context.Context, run *core.Run, before int) ([]runnerproto.DownloadFile, error) {
	sessions, err := s.runner.ListRunSessions(ctx, run.ID)
	if err != nil {
		return nil, err
	}
	parts := []string{partWorkspace, partHarnessState}
	byPart := map[string][]runnerproto.DownloadFile{}
	found := map[string]bool{}
	// The store lists the lowest number first. Walk from the newest.
	for i := len(sessions) - 1; i >= 0 && len(found) < len(parts); i-- {
		n := sessions[i].Session
		if n >= before {
			continue
		}
		events, err := s.runner.ListRunnerEvents(ctx, run.ID, n, 0, 0)
		if err != nil {
			return nil, err
		}
		latest := map[string][]string{}
		for _, ev := range events {
			if ev.Type != string(runnerproto.EventSnapshotSaved) {
				continue
			}
			var d runnerproto.SnapshotSavedData
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				s.log.Warn("runnerapi: bad snapshot.saved event", "run", run.ID, "session", n, "seq", ev.Seq, "err", err)
				continue
			}
			latest[d.Part] = d.UploadIDs
		}
		for _, part := range parts {
			uploads, ok := latest[part]
			if !ok || found[part] {
				continue
			}
			files, err := s.partFiles(ctx, run, n, part, uploads)
			if err != nil {
				return nil, err
			}
			byPart[part], found[part] = files, true
		}
	}
	out := []runnerproto.DownloadFile{}
	for _, part := range parts {
		out = append(out, byPart[part]...)
	}
	return out, nil
}

// partFiles presigns the files of one snapshot part. It skips an upload that
// has a bad ID or no object, and logs a warning.
func (s *Service) partFiles(ctx context.Context, run *core.Run, session int, part string, uploads []string) ([]runnerproto.DownloadFile, error) {
	prefix := objectPrefix(run, session, runnerproto.UploadSnapshot, part)
	var out []runnerproto.DownloadFile
	for _, id := range uploads {
		key, ok := s.parseUploadID(id)
		name, inPart := strings.CutPrefix(key, prefix)
		if !ok || !ownsKey(run, key) || !inPart || name == "" {
			s.log.Warn("runnerapi: skip a bad snapshot upload ID", "run", run.ID, "session", session, "part", part)
			continue
		}
		info, err := s.snapFiles.Stat(ctx, key)
		if errors.Is(err, objectstore.ErrNotFound) {
			s.log.Warn("runnerapi: snapshot object is missing", "run", run.ID, "session", session, "key", key)
			continue
		}
		if err != nil {
			return nil, err
		}
		url, err := s.snapFiles.PresignGet(ctx, key, s.ttl())
		if err != nil {
			return nil, err
		}
		out = append(out, runnerproto.DownloadFile{
			Part: part, Name: name, URL: url, Bytes: info.Size, ExpiresAt: s.now().Add(s.ttl()),
		})
	}
	return out, nil
}

// attachmentDownload answers kind=attachment&id=...: one presigned URL for an
// attachment of a message of the run. An attachment that no message of the
// run names, or that belongs to another team, is not found.
func (s *Service) attachmentDownload(w http.ResponseWriter, r *http.Request, runID string) {
	files := s.attachFiles
	if files == nil {
		files = s.files
	}
	if s.attachments == nil || files == nil {
		s.writeError(w, errNotImplemented())
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		s.writeError(w, errBad("id is required"))
		return
	}
	notFound := &apiError{status: http.StatusNotFound, code: codeNotFound, msg: "attachment not found"}
	ctx := r.Context()
	run, err := s.store.GetRun(ctx, runID)
	if err != nil {
		s.writeError(w, s.errInternal("get run", err))
		return
	}
	inputs, err := s.runner.ListRunInputs(ctx, runID, 0)
	if err != nil {
		s.writeError(w, s.errInternal("list run inputs", err))
		return
	}
	named := false
	for _, in := range inputs {
		if slices.Contains(in.Attachments, id) {
			named = true
			break
		}
	}
	if !named {
		s.writeError(w, notFound)
		return
	}
	att, err := s.attachments(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		s.writeError(w, notFound)
		return
	}
	if err != nil {
		s.writeError(w, s.errInternal("get attachment", err))
		return
	}
	if att.TeamID != teamOf(run) || att.Key == "" {
		s.writeError(w, notFound)
		return
	}
	info, err := files.Stat(ctx, att.Key)
	if errors.Is(err, objectstore.ErrNotFound) {
		s.writeError(w, notFound)
		return
	}
	if err != nil {
		s.writeError(w, s.errInternal("stat attachment", err))
		return
	}
	url, err := files.PresignGet(ctx, att.Key, s.ttl())
	if err != nil {
		s.writeError(w, s.errInternal("presign attachment", err))
		return
	}
	s.writeJSON(w, http.StatusOK, runnerproto.DownloadsResponse{Files: []runnerproto.DownloadFile{{
		Part: "attachment", Name: att.Name, URL: url, Bytes: info.Size, ExpiresAt: s.now().Add(s.ttl()),
	}}})
}
