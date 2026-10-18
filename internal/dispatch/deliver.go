package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	kindSystem    = "system"
	kindLifecycle = "lifecycle"
)

func (d *Dispatcher) deliverAll(ctx context.Context, open []core.RunSession) {
	for _, s := range open {
		if s.State == core.SessionActive && s.ReadyAt != nil {
			d.deliver(ctx, s.RunID)
		}
	}
}

// toItem converts a message of the queue to an item for the runner.
// With a lookup, each attachment gets its name, content type and size. If
// the lookup fails, the attachment keeps only its ID.
func toItem(ctx context.Context, lookup AttachmentLookup, m core.RunInput) runnerproto.InputItem {
	item := runnerproto.InputItem{
		MessageID:   m.ID,
		Kind:        runnerproto.MessageKind(m.Kind),
		Sender:      runnerproto.Sender{Type: m.Sender.Type, ID: m.Sender.ID, Name: m.Sender.Name},
		Content:     m.Content,
		Data:        m.Data,
		Untrusted:   m.Untrusted,
		Attachments: []runnerproto.Attachment{},
	}
	for _, id := range m.Attachments {
		a := runnerproto.Attachment{ID: id}
		if lookup != nil {
			if found, err := lookup(ctx, id); err != nil {
				slog.Warn("dispatch: attachment lookup", "run", m.RunID, "message", m.ID, "attachment", id, "err", err)
			} else {
				a.Name, a.ContentType, a.Bytes = found.Name, found.ContentType, found.Bytes
			}
		}
		item.Attachments = append(item.Attachments, a)
	}
	return item
}

func toItems(ctx context.Context, lookup AttachmentLookup, msgs []core.RunInput) ([]runnerproto.InputItem, []string) {
	items := make([]runnerproto.InputItem, 0, len(msgs))
	ids := make([]string, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, toItem(ctx, lookup, m))
		ids = append(ids, m.ID)
	}
	return items, ids
}

// messageStartsTurn reports whether a message starts a turn. System messages
// never do. A lifecycle message does, except the event "started".
func messageStartsTurn(m core.RunInput) bool {
	switch m.Kind {
	case kindSystem:
		return false
	case kindLifecycle:
		var data struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(m.Data, &data) == nil && data.Event == "started" {
			return false
		}
	}
	return true
}

// startsTurn reports whether any of the messages starts a turn.
func startsTurn(msgs []core.RunInput) bool {
	for _, m := range msgs {
		if messageStartsTurn(m) {
			return true
		}
	}
	return false
}

func hasCapability(s *core.RunSession, c runnerproto.Capability) bool {
	for _, x := range s.Capabilities {
		if x == string(c) {
			return true
		}
	}
	return false
}

// deliver sends the pending messages of a run to the live session of the run.
//
// A message is delivered when its command is stored.
func (d *Dispatcher) deliver(ctx context.Context, runID string) {
	s, err := d.cfg.Runner.LatestRunSession(ctx, runID)
	if err != nil {
		if !errors.Is(err, core.ErrNotFound) {
			d.log.Warn("dispatch: load session", "run", runID, "err", err)
		}
		return
	}
	if s.State != core.SessionActive || s.ReadyAt == nil {
		return
	}
	pending, err := d.cfg.Runner.ListRunInputs(ctx, runID, 0, core.RunInputPending)
	if err != nil {
		d.log.Warn("dispatch: list pending messages", "run", runID, "err", err)
		return
	}
	if len(pending) == 0 {
		return
	}
	switch {
	case s.ActiveTurnID == "" && startsTurn(pending):
		if d.spendStopsTurn(ctx, runID) {
			return
		}
		d.startTurn(ctx, s, pending)
	case s.ActiveTurnID != "" && hasCapability(s, runnerproto.CapMidTurnInput):
		d.midTurn(ctx, s, pending)
	}
	// Otherwise the messages wait for the next turn.
}

func (d *Dispatcher) startTurn(ctx context.Context, s *core.RunSession, pending []core.RunInput) {
	c := d.getCommander()
	if c == nil {
		d.log.Error("dispatch: no commander, cannot start a turn", "run", s.RunID)
		return
	}
	turnID := core.NewID("turn")
	// The version check of this write is the guard between replicas.
	s.ActiveTurnID = turnID
	if err := d.cfg.Runner.UpdateRunSession(ctx, s); err != nil {
		if !errors.Is(err, core.ErrConflict) {
			d.log.Warn("dispatch: claim turn", "run", s.RunID, "err", err)
		}
		return
	}
	items, ids := toItems(ctx, d.attachmentLookupFn(), pending)
	d.addEventFiles(ctx, pending, items)
	// Mark the messages before the command exists. The runner can report
	// turn.started at once, and that report accepts only delivered messages.
	if _, err := d.cfg.Runner.MarkRunInputsDelivered(ctx, s.RunID, ids, s.Session, turnID); err != nil {
		d.log.Warn("dispatch: mark messages delivered", "run", s.RunID, "err", err)
		d.releaseTurn(ctx, s, turnID)
		return
	}
	if _, err := c.EnqueueCommand(ctx, s.RunID, s.Session, runnerproto.CommandTurnStart,
		runnerproto.TurnStartData{TurnID: turnID, Input: items}); err != nil {
		d.log.Warn("dispatch: enqueue turn.start", "run", s.RunID, "session", s.Session, "err", err)
		// No message of this turn is accepted, so all go back to pending.
		if _, _, err := d.cfg.Runner.FinishRunInputsOfTurn(ctx, s.RunID, turnID); err != nil {
			d.log.Warn("dispatch: return messages", "run", s.RunID, "err", err)
		}
		d.releaseTurn(ctx, s, turnID)
	}
}

// releaseTurn takes back a turn ID when its command could not be stored.
func (d *Dispatcher) releaseTurn(ctx context.Context, s *core.RunSession, turnID string) {
	_, _, err := d.updateSession(ctx, s.RunID, s.Session, func(x *core.RunSession) bool {
		if x.ActiveTurnID != turnID {
			return false
		}
		x.ActiveTurnID = ""
		return true
	})
	if err != nil {
		d.log.Warn("dispatch: release turn", "run", s.RunID, "err", err)
	}
}

func (d *Dispatcher) midTurn(ctx context.Context, s *core.RunSession, pending []core.RunInput) {
	c := d.getCommander()
	if c == nil {
		d.log.Error("dispatch: no commander, cannot deliver messages", "run", s.RunID)
		return
	}
	// A write without a change takes the version, so that only one replica delivers.
	if err := d.cfg.Runner.UpdateRunSession(ctx, s); err != nil {
		if !errors.Is(err, core.ErrConflict) {
			d.log.Warn("dispatch: lock session", "run", s.RunID, "err", err)
		}
		return
	}
	turnID := s.ActiveTurnID
	items, ids := toItems(ctx, d.attachmentLookupFn(), pending)
	d.addEventFiles(ctx, pending, items)
	// Mark first, for the same reason as in startTurn. If the command is not
	// stored, the messages go back to pending when the turn ends.
	if _, err := d.cfg.Runner.MarkRunInputsDelivered(ctx, s.RunID, ids, s.Session, turnID); err != nil {
		d.log.Warn("dispatch: mark messages delivered", "run", s.RunID, "err", err)
		return
	}
	if _, err := c.EnqueueCommand(ctx, s.RunID, s.Session, runnerproto.CommandMessageDeliver,
		runnerproto.MessageDeliverData{Input: items}); err != nil {
		d.log.Warn("dispatch: enqueue message.deliver", "run", s.RunID, "session", s.Session, "err", err)
	}
	// The turn can end between the read and the mark. Then nobody closes
	// these messages, so close the turn again: they go back to pending.
	cur, err := d.cfg.Runner.GetRunSession(ctx, s.RunID, s.Session)
	if err == nil && cur.ActiveTurnID != turnID {
		if _, _, err := d.cfg.Runner.FinishRunInputsOfTurn(ctx, s.RunID, turnID); err != nil {
			d.log.Warn("dispatch: return messages", "run", s.RunID, "err", err)
		}
	}
}
