package api

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Stream topics accepted by GET /stream?topics=.
const (
	StreamRun        = "run"        // run state changes (RunView)
	StreamMessage    = "message"    // run transcript messages (RunMessage); requires run_id
	StreamInbox      = "inbox"      // inbox item created / resolved (InboxItem)
	StreamWorkItem   = "workitem"   // work item changes (WorkItem)
	StreamDefinition = "definition" // definition applied (Factory)
)

var allStreamTopics = []string{StreamRun, StreamMessage, StreamInbox, StreamWorkItem, StreamDefinition}

// handleStream is a team-scoped Server-Sent Events feed of live changes for the
// dashboard, fed by the event bus:
//
//	event: ready       data: {"topics": [...]}
//	event: run         data: RunView
//	event: message     data: RunMessage   (id: <seq>; only with run_id)
//	event: inbox       data: InboxItem
//	event: workitem    data: WorkItem
//	event: definition  data: Factory
//
// Query: run_id narrows run/message events to one run (messages are only sent
// when run_id is set); factory_id narrows every event to one factory; topics
// is a comma-separated subset of run,message,inbox,workitem,definition.
// With run_id, messages after Last-Event-ID (or ?after=) are replayed first.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	if s.opts.Bus == nil {
		return featureUnavailable("live updates (event bus)")
	}
	q := r.URL.Query()
	topics := qList(r, "topics")
	if len(topics) == 0 {
		topics = allStreamTopics
	}
	for _, t := range topics {
		if !slices.Contains(allStreamTopics, t) {
			return errInvalid("unknown topic %q (use %s)", t, strings.Join(allStreamTopics, ","))
		}
	}
	var run *core.Run
	if id := q.Get("run_id"); id != "" {
		var err error
		if run, err = s.runFor(ctx, p, id); err != nil {
			return err
		}
	}
	factoryID := q.Get("factory_id")
	if factoryID != "" {
		if _, err := s.factoryFor(ctx, p, factoryID); err != nil {
			return err
		}
	}
	after, err := qInt64(r, "after")
	if err != nil {
		return err
	}
	if lei := r.Header.Get("Last-Event-ID"); lei != "" {
		if n, err := strconv.ParseInt(lei, 10, 64); err == nil && n > after {
			after = n
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return Errf(CodeFeatureNotAvailable, "streaming is not supported by this connection")
	}

	want := func(t string) bool { return slices.Contains(topics, t) }
	type sub struct {
		topic string
		ch    <-chan any
	}
	var (
		subs   []sub
		unsubs []func()
	)
	subscribe := func(name, topic string) {
		if want(name) {
			ch, unsub := s.opts.Bus.Subscribe(topic)
			subs = append(subs, sub{name, ch})
			unsubs = append(unsubs, unsub)
		}
	}
	subscribe(StreamRun, core.TopicRunUpdated)
	if run != nil {
		subscribe(StreamMessage, core.TopicRunMessage)
	}
	subscribe(StreamInbox, core.TopicInbox)
	subscribe(StreamWorkItem, core.TopicWorkItem)
	subscribe(StreamDefinition, core.TopicDefinition)
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	st := &sseStream{w: w, f: flusher, lastSeq: after}
	st.send("ready", "", map[string]any{"topics": topics})
	if run != nil && want(StreamMessage) {
		if err := s.replayMessages(ctx, st, run.ID); err != nil {
			st.send("error", "", map[string]string{"message": err.Error()})
			return nil
		}
	}

	teams := &teamCache{s: s, m: map[string]string{}}
	inScope := func(fid string) bool {
		if factoryID != "" && fid != factoryID {
			return false
		}
		return fid != "" && teams.teamOf(ctx, fid) == p.TeamID
	}

	cases := make(chan streamEvent, 64)
	var wg sync.WaitGroup
	fwdCtx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); wg.Wait() }()
	for _, sb := range subs {
		wg.Add(1)
		go func(sb sub) {
			defer wg.Done()
			for {
				select {
				case <-fwdCtx.Done():
					return
				case v, ok := <-sb.ch:
					if !ok {
						return
					}
					select {
					case cases <- streamEvent{sb.topic, v}:
					case <-fwdCtx.Done():
						return
					}
				}
			}
		}(sb)
	}

	heartbeat := time.NewTicker(s.opts.SSEHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			st.comment("ping")
		case ev := <-cases:
			switch v := ev.payload.(type) {
			case *core.Run:
				if v.TeamID != p.TeamID || (run != nil && v.ID != run.ID) || (factoryID != "" && v.FactoryID != factoryID) {
					continue
				}
				st.send(StreamRun, "", s.runView(v))
			case *core.RunMessage:
				if run == nil || v.RunID != run.ID {
					continue
				}
				if v.Seq > st.lastSeq+1 {
					_ = s.replayMessages(ctx, st, run.ID) // gap: catch up from the store
				} else if v.Seq > st.lastSeq {
					st.lastSeq = v.Seq
					st.send(StreamMessage, strconv.FormatInt(v.Seq, 10), v)
				}
			case *core.InboxItem:
				if inScope(v.FactoryID) {
					st.send(StreamInbox, "", v)
				}
			case *core.WorkItem:
				if inScope(v.FactoryID) {
					st.send(StreamWorkItem, "", s.workItemView(v))
				}
			case *core.Factory:
				if v.TeamID == p.TeamID && (factoryID == "" || v.ID == factoryID) {
					st.send(StreamDefinition, "", s.factoryView(v))
				}
			}
		}
	}
}

type streamEvent struct {
	topic   string
	payload any
}

// teamCache memoizes factory -> team lookups for one stream.
type teamCache struct {
	s *Server
	m map[string]string
}

func (c *teamCache) teamOf(ctx context.Context, factoryID string) string {
	if t, ok := c.m[factoryID]; ok {
		return t
	}
	t := ""
	if f, err := c.s.opts.Store.GetFactory(ctx, factoryID); err == nil {
		t = f.TeamID
	}
	c.m[factoryID] = t
	return t
}
