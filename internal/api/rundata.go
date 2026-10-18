package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
)

// RunDataStore is the object storage holding run transcripts, artifacts and
// prompt attachments. objectstore.RunData implements it.
type RunDataStore interface {
	ListArtifacts(ctx context.Context, runID string) ([]objectstore.ObjectInfo, error)
	ArtifactURL(ctx context.Context, runID, name string, ttl time.Duration) (string, error)
	OpenArtifact(ctx context.Context, runID, name string) (io.ReadCloser, objectstore.ObjectInfo, error)
	ListAttachments(ctx context.Context, runID string) ([]objectstore.ObjectInfo, error)
	AttachmentURL(ctx context.Context, runID, name string, ttl time.Duration) (string, error)
	OpenAttachment(ctx context.Context, runID, name string) (io.ReadCloser, objectstore.ObjectInfo, error)
	AttachmentUploadURL(ctx context.Context, runID, name string, ttl time.Duration, contentType string) (string, error)
}

// RunObject is one stored artifact or attachment of a run.
type RunObject struct {
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
	// URL is a short-lived presigned download URL (absent when the store
	// cannot presign; download through the API path instead).
	URL string `json:"url,omitempty"`
}

func (s *Server) presignTTL() time.Duration {
	if s.opts.PresignTTL > 0 {
		return s.opts.PresignTTL
	}
	return objectstore.DefaultPresignTTL
}

func (s *Server) runDataRun(r *http.Request, p *Principal) (string, error) {
	if s.opts.RunData == nil {
		return "", featureUnavailable("run data storage")
	}
	run, err := s.runFor(r.Context(), p, r.PathValue("runId"))
	if err != nil {
		return "", err
	}
	return run.ID, nil
}

func objectName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name != path.Clean(name) || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "..") || strings.Contains(name, "\\") {
		return "", errInvalid("invalid object name %q", raw)
	}
	return name, nil
}

func mapObjectErr(err error, what string) error {
	switch {
	case errors.Is(err, objectstore.ErrNotFound):
		return errNotFound(what)
	case errors.Is(err, objectstore.ErrInvalidKey):
		return errInvalid("%s: invalid name", what)
	}
	return err
}

// handleRunTranscript streams the transcript of a run as JSON Lines: one run
// message per line, in order. The messages come from the run messages that
// the dispatcher stored from the runner events. The runner redacts the events
// before it sends them, so the transcript has no secret values. The raw
// harness transcript that the runner uploads is a different file, in the
// format of the harness; this endpoint does not serve it.
func (s *Server) handleRunTranscript(w http.ResponseWriter, r *http.Request, p *Principal) error {
	run, err := s.runFor(r.Context(), p, r.PathValue("runId"))
	if err != nil {
		return err
	}
	msgs, err := s.runMessagesAfter(r.Context(), run.ID, 0)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w) // Encode ends each value with a newline
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return nil //nolint:nilerr // the client went away; the status is already sent
		}
	}
	return nil
}

type objectLister func(ctx context.Context, runID string) ([]objectstore.ObjectInfo, error)
type objectPresigner func(ctx context.Context, runID, name string, ttl time.Duration) (string, error)
type objectOpener func(ctx context.Context, runID, name string) (io.ReadCloser, objectstore.ObjectInfo, error)

func (s *Server) listRunObjects(w http.ResponseWriter, r *http.Request, p *Principal, key string, list objectLister, presign objectPresigner) error {
	runID, err := s.runDataRun(r, p)
	if err != nil {
		return err
	}
	infos, err := list(r.Context(), runID)
	if err != nil {
		return mapObjectErr(err, key+" of run "+runID)
	}
	out := make([]RunObject, 0, len(infos))
	for _, in := range infos {
		// Key is the object name relative to the run's category prefix; it
		// may contain "/" (artifacts keep their directory layout).
		o := RunObject{Name: in.Key, Size: in.Size, ContentType: in.ContentType, UpdatedAt: in.LastModified.UTC()}
		if u, err := presign(r.Context(), runID, o.Name, s.presignTTL()); err == nil {
			o.URL = u
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{key: out})
	return nil
}

// getRunObject redirects to a presigned URL, or streams the object through
// the API when the store cannot presign.
func (s *Server) getRunObject(w http.ResponseWriter, r *http.Request, p *Principal, what string, presign objectPresigner, open objectOpener) error {
	runID, err := s.runDataRun(r, p)
	if err != nil {
		return err
	}
	name, err := objectName(r.PathValue("name"))
	if err != nil {
		return err
	}
	u, err := presign(r.Context(), runID, name, s.presignTTL())
	if err == nil {
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, u, http.StatusFound)
		return nil
	}
	if !errors.Is(err, objectstore.ErrPresignUnsupported) {
		return mapObjectErr(err, what+" "+name)
	}
	rc, info, err := open(r.Context(), runID, name)
	if err != nil {
		return mapObjectErr(err, what+" "+name)
	}
	defer rc.Close()
	ct := info.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, "")+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return nil
}

func (s *Server) handleListRunArtifacts(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.RunData == nil {
		return featureUnavailable("run data storage")
	}
	return s.listRunObjects(w, r, p, "artifacts", s.opts.RunData.ListArtifacts, s.opts.RunData.ArtifactURL)
}

func (s *Server) handleGetRunArtifact(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.RunData == nil {
		return featureUnavailable("run data storage")
	}
	return s.getRunObject(w, r, p, "artifact", s.opts.RunData.ArtifactURL, s.opts.RunData.OpenArtifact)
}

func (s *Server) handleListRunAttachments(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.RunData == nil {
		return featureUnavailable("run data storage")
	}
	return s.listRunObjects(w, r, p, "attachments", s.opts.RunData.ListAttachments, s.opts.RunData.AttachmentURL)
}

func (s *Server) handleGetRunAttachment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if s.opts.RunData == nil {
		return featureUnavailable("run data storage")
	}
	return s.getRunObject(w, r, p, "attachment", s.opts.RunData.AttachmentURL, s.opts.RunData.OpenAttachment)
}

// handleCreateRunAttachment returns a presigned PUT URL the client uploads a
// prompt attachment to.
func (s *Server) handleCreateRunAttachment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	runID, err := s.runDataRun(r, p)
	if err != nil {
		return err
	}
	var req struct {
		Name        string `json:"name"`
		ContentType string `json:"content_type"`
	}
	if err := decodeJSON(r, &req, false); err != nil {
		return err
	}
	name, err := objectName(req.Name)
	if err != nil {
		return err
	}
	ttl := s.presignTTL()
	u, err := s.opts.RunData.AttachmentUploadURL(r.Context(), runID, name, ttl, req.ContentType)
	if err != nil {
		if errors.Is(err, objectstore.ErrPresignUnsupported) {
			return Errf(CodeOperationNotSupported, "the attachment store cannot issue upload URLs")
		}
		return mapObjectErr(err, "attachment "+name)
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"name": name, "upload_url": u, "method": http.MethodPut,
		"expires_at": s.now().Add(ttl).UTC(),
	})
	return nil
}

// runMessagesAfter lists a run's messages with Seq > after.
func (s *Server) runMessagesAfter(ctx context.Context, runID string, after int64) ([]core.RunMessage, error) {
	return s.opts.Store.ListRunMessages(ctx, runID, after)
}
