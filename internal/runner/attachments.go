package runner

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	attachmentsDir     = ".openfactory/attachments"
	attachmentTimeout  = 2 * time.Minute
	attachmentDiagCode = "attachment/download_failed"
	inputFileDiagCode  = "input/file_write_failed"
)

// fetchAttachments downloads the attachments of the items into the work
// directory of the turns. An attachment with a Path
// goes to that path. Each other attachment goes to
// .openfactory/attachments/<message_id>/<name>. The Path of a downloaded
// attachment is the path relative to the work directory. A failure sends a
// diagnostic and leaves the Path empty. It returns new items and does not
// change the argument.
func (r *runner) fetchAttachments(ctx context.Context, items []runnerproto.InputItem) []runnerproto.InputItem {
	out := make([]runnerproto.InputItem, len(items))
	copy(out, items)
	for i := range out {
		if len(out[i].Files) > 0 {
			r.writeInputFiles(out[i])
			out[i].Files = nil
		}
		if len(out[i].Attachments) == 0 {
			continue
		}
		atts := make([]runnerproto.Attachment, len(out[i].Attachments))
		copy(atts, out[i].Attachments)
		for j := range atts {
			rel, err := r.fetchAttachment(ctx, out[i].MessageID, atts[j])
			if err != nil {
				r.log.Warn("download an attachment", "attachment", atts[j].ID, "error", scrub(err))
				r.diagnostic(attachmentDiagCode, fmt.Sprintf("attachment %s (%s) could not be downloaded: %s", atts[j].ID, atts[j].Name, scrub(err)))
				atts[j].Path = ""
				continue
			}
			atts[j].Path = rel
		}
		out[i].Attachments = atts
	}
	return out
}

// fetchAttachment downloads one attachment and returns its path relative to
// the work directory.
func (r *runner) fetchAttachment(ctx context.Context, messageID string, a runnerproto.Attachment) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, attachmentTimeout)
	defer cancel()
	resp, err := r.client.Downloads(ctx, "attachment", a.ID)
	if err != nil {
		return "", err
	}
	if len(resp.Files) != 1 {
		return "", fmt.Errorf("the control plane returned %d files, want 1", len(resp.Files))
	}
	f := resp.Files[0]
	rel := a.Path
	if rel == "" {
		name := a.Name
		if name == "" {
			name = f.Name
		}
		name = path.Base(strings.ReplaceAll(name, `\`, "/"))
		if name == "." || name == ".." || name == "/" {
			return "", fmt.Errorf("bad attachment name %q", name)
		}
		if err := checkRelPath(messageID); err != nil {
			return "", fmt.Errorf("message ID: %w", err)
		}
		rel = attachmentsDir + "/" + messageID + "/" + name
	}
	if err := checkRelPath(rel); err != nil {
		return "", err
	}
	rel = path.Clean(strings.ReplaceAll(rel, `\`, "/"))
	if err := r.fetchFile(ctx, filepath.Join(r.turnDir, filepath.FromSlash(rel)), f); err != nil {
		return "", err
	}
	return rel, nil
}

// attachmentLines renders one line for each downloaded attachment.
func attachmentLines(atts []runnerproto.Attachment) []string {
	var lines []string
	for _, a := range atts {
		if a.Path == "" {
			continue
		}
		lines = append(lines, fmt.Sprintf("Attachment: %s (%s, %d bytes)", a.Path, a.ContentType, a.Bytes))
	}
	return lines
}

// writeInputFiles writes the inline files of an item (the event
// context) into the work directory of the turns. It applies the
// redactor. A file that cannot be written sends a diagnostic and does not
// stop the turn.
func (r *runner) writeInputFiles(item runnerproto.InputItem) {
	terms := redactTerms(r.redact)
	for _, f := range item.Files {
		rel := path.Clean(strings.ReplaceAll(f.Path, `\`, "/"))
		if err := checkRelPath(rel); err != nil {
			r.diagnostic(inputFileDiagCode, fmt.Sprintf("file %q of message %s was not written: %s", f.Path, item.MessageID, err))
			continue
		}
		dst := filepath.Join(r.turnDir, filepath.FromSlash(rel))
		data := []byte(redactString(string(f.Data), terms))
		err := os.MkdirAll(filepath.Dir(dst), 0o755)
		if err == nil {
			err = os.WriteFile(dst, data, 0o644)
		}
		if err != nil {
			r.log.Warn("write an input file", "path", rel, "error", scrub(err))
			r.diagnostic(inputFileDiagCode, fmt.Sprintf("file %q of message %s was not written: %s", rel, item.MessageID, scrub(err)))
		}
	}
}
