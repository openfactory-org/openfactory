package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/objectstore"
)

// CodePayloadTooLarge is the error code of a body over its size limit.
const CodePayloadTooLarge = "payload_too_large"

func init() {
	errorSpecs[CodePayloadTooLarge] = errorSpec{http.StatusRequestEntityTooLarge, "The request body is too large.", false}
}

// attachmentFiles is implemented by *objectstore.RunData.
type attachmentFiles interface {
	Store(c objectstore.Category) objectstore.Store
}

// attachmentStore returns the object store of the attachments: the artifacts
// store of the run data.
func (s *Server) attachmentStore() (objectstore.Store, error) {
	f, ok := s.opts.RunData.(attachmentFiles)
	if !ok || f == nil {
		return nil, featureUnavailable("attachment storage")
	}
	st := f.Store(objectstore.Artifacts)
	if st == nil {
		return nil, featureUnavailable("attachment storage")
	}
	return st, nil
}

// handleUploadAttachment stores the request body as an attachment.
func (s *Server) handleUploadAttachment(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	st, err := s.attachmentStore()
	if err != nil {
		return err
	}
	name, err := core.NormalizeAttachmentName(r.URL.Query().Get("name"))
	if err != nil {
		return err
	}
	tooLarge := Errf(CodePayloadTooLarge, "an attachment is limited to %d bytes", core.MaxAttachmentBytes)
	if r.ContentLength > core.MaxAttachmentBytes {
		return tooLarge
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, core.MaxAttachmentBytes))
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return tooLarge
		}
		return errInvalid("reading body: %v", err)
	}
	ct := strings.TrimSpace(r.Header.Get("Content-Type"))
	if ct == "" {
		ct = "application/octet-stream"
	}
	id := core.NewID("att")
	key := "attachments/" + p.TeamID + "/" + id + "/" + name
	if err := objectstore.ValidateKey(key); err != nil {
		return errInvalid("invalid attachment name: %v", err)
	}
	if err := st.Put(ctx, key, bytes.NewReader(data), int64(len(data)), ct); err != nil {
		return err
	}
	now := time.Now().UTC()
	if s.opts.Now != nil {
		now = s.opts.Now().UTC()
	}
	att := core.Attachment{
		ID: id, TeamID: p.TeamID, Name: name, ContentType: ct, Bytes: int64(len(data)),
		Key: key, CreatedBy: p.ActorID(), CreatedAt: now,
	}
	meta, err := core.MarshalAttachment(att)
	if err != nil {
		return err
	}
	if err := s.opts.Store.KVSet(ctx, core.AttachmentKVKey(id), meta); err != nil {
		_ = st.Delete(ctx, key)
		return err
	}
	writeJSON(w, http.StatusCreated, att)
	return nil
}

// checkAttachments makes sure each attachment ID exists and belongs to the team.
func (s *Server) checkAttachments(ctx context.Context, teamID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > core.MaxAttachmentsPerMessage {
		return errInvalid("a message can have at most %d attachments", core.MaxAttachmentsPerMessage)
	}
	for _, id := range ids {
		raw, err := s.opts.Store.KVGet(ctx, core.AttachmentKVKey(id))
		if err != nil {
			if isNotFound(err) {
				return errInvalid("attachment %q does not exist", id)
			}
			return err
		}
		att, err := core.UnmarshalAttachment(raw)
		if err != nil || att.TeamID != teamID {
			return errInvalid("attachment %q does not exist", id)
		}
	}
	return nil
}
