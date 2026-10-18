package eventctx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// The review-comment context. The payload of a
// review comment event has the comment and a hunk of the diff. This builder
// reads the whole review thread and the whole diff of the file from the
// forge API.

// MaxReviewFileBytes is the most bytes of one file that the review builder
// writes (the same limit as the payload).
const MaxReviewFileBytes = MaxPayloadBytes

// Names of the files that the review builder writes, relative to context/.
const (
	reviewThreadFile = "review_thread.json"
	fileDiffFile     = "file_diff.patch"
)

// ForgeSource gives the forge client of one repository of a team
// (core.ForgeResolver; *forge.Resolver implements it). The builder uses the
// record of the owner of the repository of the event, of the factory of the
// event or else of the team.
type ForgeSource = core.ForgeResolver

// ReviewBuilders returns the review-comment context builders for the forges
// GitHub, GitLab and Azure DevOps. Register them with the engine. A forge
// client that does not implement core.ReviewContextReader gives no thread
// and no diff, and the envelope lists them as left out.
func ReviewBuilders(src ForgeSource) []Builder {
	return []Builder{
		reviewBuilder{provider: "github", codeForge: "GITHUB", src: src},
		reviewBuilder{provider: "gitlab", codeForge: "GITLAB", src: src},
		reviewBuilder{provider: "azure_devops", codeForge: "AZURE_DEVOPS", src: src},
	}
}

type reviewBuilder struct {
	provider  string
	codeForge string
	src       ForgeSource
}

func (b reviewBuilder) Provider() string { return b.provider }

// reviewThreadFileData is the content of context/review_thread.json.
type reviewThreadFileData struct {
	core.ReviewThread
	// OmittedComments is the number of older comments that are not in the
	// file because of the size limit.
	OmittedComments int `json:"omitted_comments,omitempty"`
}

func (b reviewBuilder) EventContext(ctx context.Context, ev *core.Event) ([]File, []Omitted, error) {
	src := ev.Source
	// Only a comment in a review thread of a pull request has a thread.
	if src.ThreadRef == "" || src.Number <= 0 || src.Repo == "" || (src.Kind != "" && src.Kind != "pull_request") {
		return nil, nil, nil
	}
	owner, repo, ok := strings.Cut(src.Repo, "/")
	if !ok || owner == "" || repo == "" {
		return nil, nil, nil
	}
	leftOut := func(reason string) ([]File, []Omitted, error) {
		return nil, []Omitted{
			{Name: "context/" + reviewThreadFile, Reason: reason},
			{Name: "context/" + fileDiffFile, Reason: reason},
		}, nil
	}
	f, err := b.forge(ctx, ev)
	if err != nil {
		return leftOut("no forge client: " + oneLine(err.Error()))
	}
	rc, ok := f.(core.ReviewContextReader)
	if !ok {
		return leftOut("the " + b.provider + " forge cannot read review threads or diffs")
	}

	var files []File
	var omitted []Omitted
	path := payloadPath(ev)
	th, err := rc.ReviewThread(ctx, owner, repo, src.Number, src.ThreadRef)
	if err != nil {
		omitted = append(omitted, Omitted{Name: "context/" + reviewThreadFile, Reason: readReason("the review thread", err)})
	} else {
		data, dropped := threadFile(th)
		files = append(files, File{Path: reviewThreadFile, Data: data})
		if dropped > 0 {
			omitted = append(omitted, Omitted{Name: "context/" + reviewThreadFile,
				Reason: fmt.Sprintf("%d older comments are left out (limit %d KiB)", dropped, MaxReviewFileBytes>>10)})
		}
		if th.Path != "" {
			path = th.Path
		}
	}

	if path == "" {
		omitted = append(omitted, Omitted{Name: "context/" + fileDiffFile, Reason: "the comment is not on a file"})
		return files, omitted, nil
	}
	d, err := rc.FileDiff(ctx, owner, repo, src.Number, path)
	if err != nil {
		omitted = append(omitted, Omitted{Name: "context/" + fileDiffFile, Reason: readReason("the diff of "+path, err)})
		return files, omitted, nil
	}
	data, left := diffFile(path, d.Patch)
	files = append(files, File{Path: fileDiffFile, Data: data})
	switch {
	case left > 0:
		omitted = append(omitted, Omitted{Name: "context/" + fileDiffFile,
			Reason: fmt.Sprintf("cut at %d KiB, %d bytes of the diff are left out", MaxReviewFileBytes>>10, left)})
	case d.Truncated:
		omitted = append(omitted, Omitted{Name: "context/" + fileDiffFile, Reason: "the forge cut the diff"})
	}
	return files, omitted, nil
}

// forge returns the forge client for the event.
func (b reviewBuilder) forge(ctx context.Context, ev *core.Event) (core.Forge, error) {
	if b.src == nil {
		return nil, errors.New("no forge source")
	}
	return b.src.Forge(ctx, core.ForgeLookup{TeamID: ev.TeamID, FactoryID: ev.FactoryID, CodeForge: b.codeForge,
		Repo: ev.Source.Repo, Scope: []string{ev.Source.Repo}})
}

// payloadPath is the file of the comment, from the payload of the event. The
// thread of the forge is the better source: Azure DevOps has no path in the
// payload.
func payloadPath(ev *core.Event) string {
	var p struct {
		Comment *struct {
			Path string `json:"path"`
		} `json:"comment"`
		ObjectAttributes *struct {
			Position *struct {
				NewPath string `json:"new_path"`
				OldPath string `json:"old_path"`
			} `json:"position"`
		} `json:"object_attributes"`
	}
	if len(ev.Payload) == 0 || json.Unmarshal(ev.Payload, &p) != nil {
		return ""
	}
	if p.Comment != nil && p.Comment.Path != "" {
		return p.Comment.Path
	}
	if o := p.ObjectAttributes; o != nil && o.Position != nil {
		if o.Position.NewPath != "" {
			return o.Position.NewPath
		}
		return o.Position.OldPath
	}
	return ""
}

// threadFile encodes the thread. When it is over the limit, it drops the
// oldest comments (the newest one is the comment of the event), then cuts the
// body of a last comment. It returns the number of dropped comments.
func threadFile(th *core.ReviewThread) ([]byte, int) {
	d := reviewThreadFileData{ReviewThread: *th}
	d.Comments = append([]core.ThreadComment(nil), th.Comments...)
	for {
		b, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return []byte("{}"), 0
		}
		if len(b) <= MaxReviewFileBytes {
			return b, d.OmittedComments
		}
		if len(d.Comments) > 1 {
			d.Comments = d.Comments[1:]
			d.OmittedComments++
			continue
		}
		// One comment is too large. Cut its body by the excess; the JSON
		// escapes can double the size, so cut by twice the excess.
		c := &d.Comments[0]
		if c.Body == "" {
			return []byte("{}"), d.OmittedComments // the metadata alone is over the limit
		}
		cutTo := len(c.Body) - 2*(len(b)-MaxReviewFileBytes) - 64
		if cutTo < 0 {
			cutTo = 0
		}
		c.Body = string(cut([]byte(c.Body), cutTo))
	}
}

// diffFile makes the file of a diff with the file header. It cuts the diff at
// a line end when it is over the limit, and returns the bytes that it left out.
func diffFile(path, patch string) ([]byte, int) {
	header := "--- a/" + path + "\n+++ b/" + path + "\n"
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	room := MaxReviewFileBytes - len(header)
	if room < 0 {
		room = 0
	}
	left := 0
	if len(patch) > room {
		kept := string(cut([]byte(patch), room))
		if i := strings.LastIndex(kept, "\n"); i >= 0 {
			kept = kept[:i+1]
		}
		left = len(patch) - len(kept)
		patch = kept
	}
	return []byte(header + patch), left
}

// readReason says why a thread or a diff was not read.
func readReason(what string, err error) string {
	if errors.Is(err, core.ErrOperationNotSupported) {
		return "this forge cannot read " + what
	}
	return "could not read " + what + ": " + oneLine(err.Error())
}

// oneLine makes an error message fit one line of the prompt.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 300 {
		s = string(r[:299]) + "…"
	}
	return s
}
