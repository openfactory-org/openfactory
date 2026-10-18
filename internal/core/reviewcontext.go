package core

import (
	"context"
	"time"
)

// ---------- Review context ----------

// ThreadComment is one comment of a review thread.
type ThreadComment struct {
	ID        string    `json:"id,omitempty"`
	Author    string    `json:"author,omitempty"`
	Body      string    `json:"body,omitempty"`
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
}

// ReviewThread is a review thread of a pull request. Comments are in order,
// oldest first. Path is the file that the thread is on. It is empty for a
// discussion that is not on a file.
type ReviewThread struct {
	Ref      string          `json:"ref,omitempty"`
	Path     string          `json:"path,omitempty"`
	Line     int             `json:"line,omitempty"`
	Resolved bool            `json:"resolved,omitempty"`
	Comments []ThreadComment `json:"comments"`
}

// FileDiff is the unified diff of one file of a pull request: the hunks,
// without the file header. Truncated is true when the forge cut the patch.
type FileDiff struct {
	Path      string `json:"path"`
	Patch     string `json:"patch"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ReviewContextReader is implemented by a forge client that can read a review
// thread and the diff of a file of a pull request. The context builder of a
// review comment uses it. A client that does not
// implement it, or that returns ErrOperationNotSupported, gives no thread or
// diff; the builder then lists them as left out.
type ReviewContextReader interface {
	// ReviewThread reads the thread threadRef (SourceRef.ThreadRef).
	ReviewThread(ctx context.Context, owner, repo string, number int, threadRef string) (*ReviewThread, error)
	// FileDiff reads the diff of the file path in the pull request.
	FileDiff(ctx context.Context, owner, repo string, number int, path string) (*FileDiff, error)
}
