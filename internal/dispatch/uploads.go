package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// onArtifact stores the file of an artifact event with Config.OnUpload. It then
// adds an artifact message to the conversation.
func (d *Dispatcher) onArtifact(ctx context.Context, s *core.RunSession, e core.RunnerEvent) error {
	var data runnerproto.ArtifactData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	if data.Name == "" || data.UploadID == "" {
		return errors.New("artifact event without name or upload_id")
	}
	if d.cfg.OnUpload == nil {
		return nil
	}
	if err := d.cfg.OnUpload(ctx, s.RunID, s.Session, string(runnerproto.UploadArtifact), data.Name, data.UploadID); err != nil {
		return fmt.Errorf("store artifact %q: %w", data.Name, err)
	}
	raw, err := json.Marshal(map[string]any{"name": data.Name, "type": data.Type})
	if err != nil {
		return err
	}
	return d.appendMessage(ctx, s.RunID, "artifact", data.Name, core.RunnerEvent{Data: raw})
}

// onTranscriptSaved stores the raw transcript of a transcript.saved event with Config.OnUpload.
func (d *Dispatcher) onTranscriptSaved(ctx context.Context, s *core.RunSession, e core.RunnerEvent) error {
	var data runnerproto.TranscriptSavedData
	if err := json.Unmarshal(e.Data, &data); err != nil {
		return err
	}
	if data.UploadID == "" {
		return errors.New("transcript.saved event without upload_id")
	}
	if d.cfg.OnUpload == nil {
		return nil
	}
	if err := d.cfg.OnUpload(ctx, s.RunID, s.Session, string(runnerproto.UploadTranscript), "", data.UploadID); err != nil {
		return fmt.Errorf("store transcript: %w", err)
	}
	return nil
}
