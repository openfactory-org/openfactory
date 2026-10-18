package dispatch

import (
	"context"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/eventctx"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// addEventFiles adds the event context of each message to its item. The
// engine stores the context in the key/value store
// under the message ID. The first prompt of a run has its own queue ID, so
// the engine also stores the message ID of the prompt for the run. A message
// without a context, or a context that cannot be read, gets no files.
func (d *Dispatcher) addEventFiles(ctx context.Context, msgs []core.RunInput, items []runnerproto.InputItem) {
	for i := range msgs {
		if i >= len(items) {
			return
		}
		id := msgs[i].ID
		if msgs[i].DedupeKey == "prompt" {
			mid, err := d.cfg.Store.KVGet(ctx, eventctx.PromptKVKey(msgs[i].RunID))
			if err != nil || mid == "" {
				continue
			}
			id = mid
		}
		raw, err := d.cfg.Store.KVGet(ctx, eventctx.KVKey(id))
		if err != nil || raw == "" {
			continue
		}
		b, err := eventctx.Decode(raw)
		if err != nil {
			d.log.Warn("dispatch: read event context", "run", msgs[i].RunID, "message", id, "err", err)
			continue
		}
		for _, f := range b.Files {
			items[i].Files = append(items[i].Files, runnerproto.InputFile{Path: f.Path, Data: f.Data})
		}
	}
}
