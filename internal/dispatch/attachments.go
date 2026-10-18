package dispatch

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
)

// AttachmentLookup returns the metadata of an attachment of a message.
type AttachmentLookup func(ctx context.Context, id string) (core.Attachment, error)

// SetAttachmentLookup sets the lookup that adds the name, content type and
// size to the attachments of the items for the runner. Nil removes it.
func (d *Dispatcher) SetAttachmentLookup(fn AttachmentLookup) {
	d.mu.Lock()
	d.attachmentLookup = fn
	d.mu.Unlock()
}

func (d *Dispatcher) attachmentLookupFn() AttachmentLookup {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.attachmentLookup
}
