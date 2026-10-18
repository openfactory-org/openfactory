package runnerclient

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// EventLog is a durable, append-only log of events in one file. The file has
// one JSON object (a runnerproto.Event) on each line.
type EventLog struct {
	mu      sync.Mutex
	f       *os.File
	size    int64
	lastSeq int64
	now     func() time.Time
}

// OpenEventLog opens the log file at path and creates it with mode 0o600 if
// it does not exist. It reads the file to find the last sequence number. An
// incomplete last line, left by a crash during a write, is cut off. A bad
// line that is not the last line is an error. A nil now means time.Now.
func OpenEventLog(path string, now func() time.Time) (*EventLog, error) {
	if now == nil {
		now = time.Now
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runnerclient: open event log: %w", err)
	}
	l := &EventLog{f: f, now: now}
	if err := l.recover(); err != nil {
		f.Close()
		return nil, err
	}
	return l, nil
}

// recover reads each line, finds the last sequence number and the end of the
// last good line, and cuts off a torn last line.
func (l *EventLog) recover() error {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("runnerclient: seek event log: %w", err)
	}
	r := bufio.NewReader(l.f)
	var good int64  // end offset of the last good line
	var badAt int64 // offset of a bad line, valid if bad is true
	var bad bool
	var needNewline bool
	for lineNo := 1; ; lineNo++ {
		line, err := r.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("runnerclient: read event log: %w", err)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("runnerclient: read event log: %w", err)
		}
		if bad {
			return fmt.Errorf("runnerclient: event log line before line %d is corrupt", lineNo)
		}
		complete := bytes.HasSuffix(line, []byte("\n"))
		var e runnerproto.Event
		if jerr := json.Unmarshal(bytes.TrimSpace(line), &e); jerr != nil || e.Seq <= 0 {
			bad = true
			badAt = good
			continue
		}
		l.lastSeq = e.Seq
		good += int64(len(line))
		if !complete {
			needNewline = true
		}
	}
	if bad {
		if err := l.f.Truncate(badAt); err != nil {
			return fmt.Errorf("runnerclient: cut torn line of event log: %w", err)
		}
		good = badAt
		needNewline = false
	}
	l.size = good
	if needNewline {
		if _, err := l.f.Write([]byte("\n")); err != nil {
			return fmt.Errorf("runnerclient: repair event log: %w", err)
		}
		l.size++
	}
	return nil
}

// Append adds an event with the next sequence number and the current time
// (UTC). It calls Sync before it returns. It returns an error for an event
// type that is not valid.
func (l *EventLog) Append(typ runnerproto.EventType, turnID string, data any) (runnerproto.Event, error) {
	if !typ.Valid() {
		return runnerproto.Event{}, fmt.Errorf("runnerclient: invalid event type %q", typ)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e, err := runnerproto.NewEvent(l.lastSeq+1, typ, turnID, l.now().UTC(), data)
	if err != nil {
		return runnerproto.Event{}, fmt.Errorf("runnerclient: make event: %w", err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return runnerproto.Event{}, fmt.Errorf("runnerclient: encode event: %w", err)
	}
	line = append(line, '\n')
	if n, err := l.f.Write(line); err != nil {
		if n > 0 {
			_ = l.f.Truncate(l.size)
		}
		return runnerproto.Event{}, fmt.Errorf("runnerclient: write event: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return runnerproto.Event{}, fmt.Errorf("runnerclient: sync event log: %w", err)
	}
	l.size += int64(len(line))
	l.lastSeq = e.Seq
	return e, nil
}

// LastSeq returns the sequence number of the last event, or 0 if the log is
// empty.
func (l *EventLog) LastSeq() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.lastSeq
}

// After returns the events with a sequence number higher than seq, in order.
// It stops before the event that makes the total size of the JSON lines
// larger than maxBytes, but it returns at least one event if one exists. A
// maxBytes of 0 or less means runnerproto.DefaultEventBatchBytes.
func (l *EventLog) After(seq int64, maxBytes int) ([]runnerproto.Event, error) {
	if maxBytes <= 0 {
		maxBytes = runnerproto.DefaultEventBatchBytes
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	r := bufio.NewReader(io.NewSectionReader(l.f, 0, l.size))
	var out []runnerproto.Event
	total := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) > 0 {
				var e runnerproto.Event
				if jerr := json.Unmarshal(line, &e); jerr != nil {
					return nil, fmt.Errorf("runnerclient: decode event log line: %w", jerr)
				}
				if e.Seq > seq {
					if len(out) > 0 && total+len(line) > maxBytes {
						return out, nil
					}
					total += len(line)
					out = append(out, e)
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, fmt.Errorf("runnerclient: read event log: %w", err)
		}
	}
}

// Close closes the file of the log.
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("runnerclient: close event log: %w", err)
	}
	return nil
}
