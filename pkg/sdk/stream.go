package sdk

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"strings"
)

// Event types emitted by a run event stream.
const (
	EventRun     = "run"     // Run holds the current state
	EventMessage = "message" // Message holds a new transcript entry
	EventDone    = "done"    // Run finished (or is BLOCKED with StopOnIdle); the stream ends
)

// RunEvent is one Server-Sent Event of a run.
type RunEvent struct {
	Type    string
	ID      string
	Run     *Run
	Message *RunMessage
	Raw     json.RawMessage
}

// StreamOptions configures Runs.Stream.
type StreamOptions struct {
	// After resumes after this message sequence number.
	After int64
	// StopOnIdle ends the stream when the run is BLOCKED and waits for a person.
	StopOnIdle bool
}

// RunStream reads run events. It reconnects transparently (resuming from the
// last message) when the connection drops before the run is done.
type RunStream struct {
	c      *Client
	ctx    context.Context
	cancel context.CancelFunc
	runID  string
	opts   StreamOptions

	body    io.ReadCloser
	scanner *bufio.Reader
	lastSeq int64
	ev      RunEvent
	err     error
	done    bool
	retries int
}

// Stream opens the SSE event stream of a run.
func (s *RunsService) Stream(ctx context.Context, runID string, opts *StreamOptions) (*RunStream, error) {
	if opts == nil {
		opts = &StreamOptions{}
	}
	cctx, cancel := context.WithCancel(ctx)
	st := &RunStream{c: s.c, ctx: cctx, cancel: cancel, runID: runID, opts: *opts, lastSeq: opts.After}
	if err := st.connect(); err != nil {
		cancel()
		return nil, err
	}
	return st, nil
}

func (st *RunStream) connect() error {
	q := url.Values{}
	if st.lastSeq > 0 {
		q.Set("after", strconv.FormatInt(st.lastSeq, 10))
	}
	if st.opts.StopOnIdle {
		q.Set("stop_on_idle", "true")
	}
	hdr := map[string]string{"Cache-Control": "no-cache"}
	if st.lastSeq > 0 {
		hdr["Last-Event-ID"] = strconv.FormatInt(st.lastSeq, 10)
	}
	res, err := st.c.rawRequest(st.ctx, "GET", "runs/"+pathEscape(st.runID)+"/events", q, nil, "text/event-stream", hdr) //nolint:bodyclose // the stream keeps the body in st.body and Close closes it
	if err != nil {
		return err
	}
	st.body = res.Body
	st.scanner = bufio.NewReaderSize(res.Body, 64*1024)
	return nil
}

// Next blocks until the next event and reports whether one is available.
func (st *RunStream) Next() bool {
	for {
		if st.done || st.err != nil {
			return false
		}
		ev, err := st.readEvent()
		if err == nil {
			st.ev = ev
			if ev.Type == EventMessage && ev.Message != nil && ev.Message.Seq > st.lastSeq {
				st.lastSeq = ev.Message.Seq
			}
			if ev.Type == EventDone {
				st.done = true
			}
			st.retries = 0
			return true
		}
		if st.ctx.Err() != nil {
			st.err = st.ctx.Err()
			return false
		}
		// The connection ended before "done": reconnect from the last seq.
		_ = st.body.Close()
		if st.retries >= max(st.c.maxRetries, 1) {
			st.err = err
			return false
		}
		if serr := sleep(st.ctx, st.c.backoff(st.retries, 0)); serr != nil {
			st.err = serr
			return false
		}
		st.retries++
		if cerr := st.connect(); cerr != nil {
			st.err = cerr
			return false
		}
	}
}

// Event returns the current event.
func (st *RunStream) Event() RunEvent { return st.ev }

// Err returns the error that ended the stream (nil after a clean "done").
func (st *RunStream) Err() error { return st.err }

// Close releases the connection.
func (st *RunStream) Close() error {
	st.cancel()
	if st.body != nil {
		return st.body.Close()
	}
	return nil
}

var errStreamEnded = errors.New("openfactory: event stream ended")

func (st *RunStream) readEvent() (RunEvent, error) {
	var ev RunEvent
	var data strings.Builder
	for {
		line, err := st.scanner.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				return ev, errStreamEnded
			}
			return ev, err
		}
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ev.Type == "" && data.Len() == 0 {
				continue // comment-only block or keep-alive
			}
			if ev.Type == "" {
				ev.Type = "message"
			}
			ev.Raw = json.RawMessage(data.String())
			switch ev.Type {
			case EventRun, EventDone:
				var r Run
				if err := json.Unmarshal(ev.Raw, &r); err == nil {
					ev.Run = &r
				}
			case EventMessage:
				var m RunMessage
				if err := json.Unmarshal(ev.Raw, &m); err == nil {
					ev.Message = &m
				}
			}
			return ev, nil
		case strings.HasPrefix(line, ":"):
			// comment
		case strings.HasPrefix(line, "event:"):
			ev.Type = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "id:"):
			ev.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
}
