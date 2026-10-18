package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// uploadStorer moves the files that a runner uploaded (artifact and transcript
// events) to the places where the run data readers look.
type uploadStorer struct {
	runData *objectstore.RunData
	// keyOf resolves an upload ID to its object key (runnerapi.Service.ObjectKey).
	keyOf func(uploadID string) (string, bool)
}

// store implements dispatch.Config.OnUpload. An artifact is copied to
// runs/{run}/artifacts/{name}, where GET /runs/{id}/artifacts lists it,
// A raw harness transcript is copied to
// runs/{run}/harness/{session}.jsonl in the transcripts store. The run message
// transcript of the API keeps its own format and its own objects.
func (u *uploadStorer) store(ctx context.Context, runID string, session int, kind, name, uploadID string) error {
	if u.runData == nil {
		return errors.New("run data storage is not configured")
	}
	key, ok := u.keyOf(uploadID)
	if !ok {
		return errors.New("invalid upload ID")
	}
	prefix := "runs/" + runID + "/sessions/" + strconv.Itoa(session) + "/" + kind + "/"
	rest, ok := strings.CutPrefix(key, prefix)
	if !ok || rest == "" {
		return fmt.Errorf("upload %q does not belong to this session", key)
	}
	src := u.runData.Store(objectstore.Artifacts) // the runner API writes uploads here
	if src == nil {
		return errors.New("no store for uploads")
	}
	rc, info, err := src.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("read upload: %w", err)
	}
	defer rc.Close()
	switch runnerproto.UploadKind(kind) {
	case runnerproto.UploadArtifact:
		ct := info.ContentType
		if ct == "" {
			ct = "application/octet-stream"
		}
		_, err = u.runData.PutArtifact(ctx, runID, rest, rc, info.Size, ct)
	case runnerproto.UploadTranscript:
		_, err = u.runData.PutHarnessTranscript(ctx, runID, session, rc, info.Size)
	default:
		err = fmt.Errorf("upload kind %q", kind)
	}
	return err
}
