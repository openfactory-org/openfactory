package runnerclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	minBackoff = time.Second
	maxBackoff = 30 * time.Second
)

// SenderOptions are the settings of a Sender.
type SenderOptions struct {
	// Client sends the requests. It is required.
	Client *Client
	// Log holds the events to send. It is required.
	Log *EventLog
	// BatchBytes is the size limit of a batch. 0 means
	// runnerproto.DefaultEventBatchBytes.
	BatchBytes int
	// Sleep waits for d or until ctx ends, and then returns ctx.Err(). Nil
	// means a real sleep.
	Sleep func(ctx context.Context, d time.Duration) error
}

// Sender sends the events of an EventLog to the control plane.
type Sender struct {
	client *Client
	log    *EventLog
	batch  int
	sleep  func(ctx context.Context, d time.Duration) error

	mu    sync.Mutex
	acked int64
}

// NewSender makes a Sender. It returns an error if Client or Log is nil.
func NewSender(o SenderOptions) (*Sender, error) {
	if o.Client == nil {
		return nil, errors.New("runnerclient: sender needs a client")
	}
	if o.Log == nil {
		return nil, errors.New("runnerclient: sender needs an event log")
	}
	s := &Sender{client: o.Client, log: o.Log, batch: o.BatchBytes, sleep: o.Sleep}
	if s.sleep == nil {
		s.sleep = realSleep
	}
	return s, nil
}

func realSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Acked returns the newest sequence number that the control plane has.
func (s *Sender) Acked() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acked
}

// SetAcked sets the newest sequence number that the control plane has.
func (s *Sender) SetAcked(seq int64) {
	s.mu.Lock()
	s.acked = seq
	s.mu.Unlock()
}

// Flush sends the events that the control plane does not have yet, until
// Acked equals the last sequence number of the log. It sends again after a
// retryable error, with a delay that starts at 1 second and doubles up to 30
// seconds. It returns any other error.
func (s *Sender) Flush(ctx context.Context) error {
	delay := minBackoff
	var gapAck int64
	haveGap := false
	for {
		acked := s.Acked()
		events, err := s.log.After(acked, s.batch)
		if err != nil {
			return fmt.Errorf("runnerclient: read event log: %w", err)
		}
		if len(events) == 0 {
			return nil
		}
		ack, err := s.client.SendEvents(ctx, runnerproto.EventBatch{Events: events})
		switch {
		case err == nil:
			if ack <= acked {
				return fmt.Errorf("runnerclient: no progress: ack %d, already acked %d", ack, acked)
			}
			s.SetAcked(ack)
			delay = minBackoff
			haveGap = false
		case errors.Is(err, ErrGap):
			if haveGap && gapAck == ack {
				return err
			}
			haveGap, gapAck = true, ack
			s.SetAcked(ack)
		case IsRetryable(err):
			if serr := s.sleep(ctx, delay); serr != nil {
				return serr
			}
			delay *= 2
			if delay > maxBackoff {
				delay = maxBackoff
			}
		default:
			return err
		}
	}
}
