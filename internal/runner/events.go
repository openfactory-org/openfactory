package runner

import (
	"encoding/json"
	"time"

	"github.com/openfactory-org/openfactory/internal/redact"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const flushInterval = 2 * time.Second

// emit writes one event to the log and wakes the flusher. It removes secret
// values from the data first. An error is logged, not returned, because a
// harness can do nothing about it.
func (r *runner) emit(typ runnerproto.EventType, turnID string, data any) {
	raw, err := json.Marshal(data)
	if err != nil {
		r.log.Error("encode event", "type", typ, "error", err)
		return
	}
	raw = redactJSON(raw, r.redact)
	if _, err := r.events.Append(typ, turnID, json.RawMessage(raw)); err != nil {
		r.log.Error("append event", "type", typ, "error", err)
		return
	}
	r.signalFlush()
}

func (r *runner) signalFlush() {
	select {
	case r.flushCh <- struct{}{}:
	default:
	}
}

// redactJSON replaces each secret, and its forms (package redact), with ***.
// If the result is not valid JSON, it returns a safe replacement.
func redactJSON(raw []byte, secrets []string) []byte {
	list := redactTerms(secrets)
	if len(list) == 0 {
		return raw
	}
	return redact.JSON(raw, list)
}

// redactString replaces each term of the list with ***. The list comes from redactTerms.
func redactString(s string, list []string) string { return redact.String(s, list) }

// redactTerms lists each secret and its JSON-escaped, base64 and URL-encoded
// forms, the longest first, so that a secret inside a longer secret does not
// leave a rest. The runner removes each value that the server names, also a
// short one. The control plane makes the encoded forms of a value of 8 or
// more characters too.
func redactTerms(secrets []string) []string { return redact.Terms(secrets, 1) }

// flusher sends the events of the log when it is signaled and every 2 s.
func (r *runner) flusher() {
	defer close(r.flusherDone)
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for {
		select {
		case <-r.flushCtx.Done():
			return
		case <-t.C:
		case <-r.flushCh:
		}
		err := r.sender.Flush(r.flushCtx)
		if err == nil || r.flushCtx.Err() != nil {
			continue
		}
		if runnerclient.IsSuperseded(err) {
			r.markSuperseded()
			return
		}
		r.log.Warn("send events", "error", err)
	}
}

// stopFlusher stops the flusher and waits for it.
func (r *runner) stopFlusher() {
	r.flushStop()
	<-r.flusherDone
}
