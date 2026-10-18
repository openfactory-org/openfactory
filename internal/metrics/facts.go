package metrics

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge/eventattr"
)

// Pull request facts. RecordEvent updates the table
// pull_request_facts from forge events. It runs for every forge event,
// before the engine drops the events of the product's own bot, so the facts
// also see the pull requests and pushes of the agent.

// factsMu serializes the read-modify-write of the facts in one process.
var factsMu sync.Mutex

func forgeProvider(p string) bool {
	switch strings.ToLower(strings.ReplaceAll(strings.TrimSpace(p), "-", "_")) {
	case "github", "gitlab", "azure_devops":
		return true
	}
	return false
}

// actorKind returns the kind of the actor of an event.
func actorKind(a core.Actor) string {
	switch {
	case a.IsSelf:
		return core.PushByAgent
	case a.IsBot:
		return core.PushByBot
	}
	return core.PushByPerson
}

func attrString(ev *core.Event, k string) string {
	s, _ := ev.Attributes[k].(string)
	return s
}

func attrInt(ev *core.Event, k string) (int, bool) {
	switch v := ev.Attributes[k].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

func attrTime(ev *core.Event, k string) (time.Time, bool) {
	s := attrString(ev, k)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// eventAction returns the lower-case action attribute (GitLab merge request events).
func eventAction(ev *core.Event) string { return strings.ToLower(attrString(ev, eventattr.Action)) }

func isOpenedEvent(ev *core.Event) bool {
	switch strings.ToLower(ev.Type) {
	case "pull_request_opened", "pull_request_created":
		return true
	case "merge_request":
		return eventAction(ev) == "open"
	}
	return false
}

func isMergedEvent(ev *core.Event) bool {
	t := strings.ToLower(ev.Type)
	if strings.HasSuffix(t, "_merged") {
		return true
	}
	if m, ok := ev.Attributes[eventattr.Merged].(bool); ok && m {
		return true
	}
	return t == "merge_request" && eventAction(ev) == "merge"
}

func isClosedEvent(ev *core.Event) bool {
	t := strings.ToLower(ev.Type)
	return strings.HasSuffix(t, "_closed") || (t == "merge_request" && eventAction(ev) == "close")
}

func isReviewEvent(ev *core.Event) bool {
	t := strings.ToLower(ev.Type)
	return t == "pull_request_review_submitted" || (t == "merge_request" && eventAction(ev) == "approved")
}

func isSynchronizeEvent(ev *core.Event) bool {
	return strings.ToLower(ev.Type) == "pull_request_synchronized"
}

// RecordEvent updates the pull request facts from one forge event. A store
// without core.PullRequestFactStore, an event of another provider and an
// event without a team are skipped.
func RecordEvent(ctx context.Context, store core.Store, ev *core.Event, now time.Time) error {
	ps, ok := store.(core.PullRequestFactStore)
	if !ok || ev == nil || ev.TeamID == "" || !forgeProvider(ev.Provider) {
		return nil
	}
	at := ev.ReceivedAt.UTC()
	if at.IsZero() {
		at = now.UTC()
	}
	factsMu.Lock()
	defer factsMu.Unlock()
	if strings.ToLower(ev.Type) == "push" {
		return recordPush(ctx, ps, ev, at, now)
	}
	if ev.Source.Kind != "pull_request" || ev.Source.Number == 0 || ev.Source.Repo == "" {
		return nil
	}
	src := ev.Source
	src.ThreadRef = ""
	key := src.Key()
	id := core.PullRequestFactsID(ev.TeamID, key)
	f, err := ps.GetPullRequestFacts(ctx, id)
	if err != nil {
		if !isNotFound(err) {
			return err
		}
		f = &core.PullRequestFacts{ID: id, TeamID: ev.TeamID, Key: key, Provider: strings.ToLower(src.Provider), Repo: src.Repo, Number: src.Number}
	}
	before := *f
	if src.URL != "" {
		f.URL = src.URL
	}
	if b := attrString(ev, eventattr.HeadBranch); b != "" {
		f.HeadBranch = b
	}
	if n, ok := attrInt(ev, eventattr.Additions); ok {
		f.Additions = &n
	}
	if n, ok := attrInt(ev, eventattr.Deletions); ok {
		f.Deletions = &n
	}
	if f.OpenedAt == nil {
		if t, ok := attrTime(ev, eventattr.PRCreatedAt); ok {
			f.OpenedAt = &t
		} else if isOpenedEvent(ev) {
			t := at
			f.OpenedAt = &t
		}
	}
	if f.Opener == "" {
		switch {
		case isOpenedEvent(ev):
			f.Opener, f.OpenerKind = ev.Actor.Handle, actorKind(ev.Actor)
		case attrString(ev, eventattr.PROpener) != "":
			f.Opener = attrString(ev, eventattr.PROpener)
			isBot, _ := ev.Attributes[eventattr.PROpenerIsBot].(bool)
			switch {
			case strings.EqualFold(f.Opener, ev.Actor.Handle):
				f.OpenerKind = actorKind(ev.Actor)
			case isBot:
				f.OpenerKind = core.PushByBot
			default:
				f.OpenerKind = core.PushByPerson
			}
		}
	}
	switch {
	case isSynchronizeEvent(ev):
		addPush(f, core.PullRequestPush{At: at, Actor: ev.Actor.Handle, Kind: actorKind(ev.Actor), SHA: attrString(ev, eventattr.HeadSHA)})
	case isReviewEvent(ev):
		// The first review by someone other than the opener and the
		// product's own bot.
		if f.FirstReviewAt == nil && !ev.Actor.IsSelf && !strings.EqualFold(ev.Actor.Handle, f.Opener) {
			t := at
			f.FirstReviewAt = &t
		}
	}
	if isMergedEvent(ev) && f.MergedAt == nil {
		t := at
		if mt, ok := attrTime(ev, eventattr.MergedAt); ok {
			t = mt
		}
		f.MergedAt = &t
	} else if isClosedEvent(ev) && !isMergedEvent(ev) && f.ClosedAt == nil && f.MergedAt == nil {
		t := at
		f.ClosedAt = &t
	}
	if factsEqual(&before, f) && before.ID != "" && !before.UpdatedAt.IsZero() {
		return nil
	}
	f.UpdatedAt = now.UTC()
	return ps.PutPullRequestFacts(ctx, f)
}

// recordPush adds a push to the open pull requests whose head branch got it.
func recordPush(ctx context.Context, ps core.PullRequestFactStore, ev *core.Event, at, now time.Time) error {
	branch := attrString(ev, eventattr.Branch)
	if branch == "" || ev.Source.Repo == "" {
		return nil
	}
	open, err := ps.ListPullRequestFacts(ctx, core.PullRequestFactFilter{
		TeamID: ev.TeamID, Provider: strings.ToLower(ev.Provider), Repo: ev.Source.Repo, HeadBranch: branch, OpenOnly: true,
	})
	if err != nil {
		return err
	}
	for i := range open {
		f := &open[i]
		if f.OpenedAt != nil && at.Before(*f.OpenedAt) {
			continue
		}
		if !addPush(f, core.PullRequestPush{At: at, Actor: ev.Actor.Handle, Kind: actorKind(ev.Actor), SHA: attrString(ev, eventattr.HeadSHA)}) {
			continue
		}
		f.UpdatedAt = now.UTC()
		if err := ps.PutPullRequestFacts(ctx, f); err != nil {
			return err
		}
	}
	return nil
}

// addPush appends p unless a push with the same commit (or the same time
// and actor) is recorded. A forge can report one push twice: as a push and
// as a pull request update.
func addPush(f *core.PullRequestFacts, p core.PullRequestPush) bool {
	for _, x := range f.Pushes {
		if (p.SHA != "" && x.SHA == p.SHA) || (p.SHA == "" && x.At.Equal(p.At) && x.Actor == p.Actor) {
			return false
		}
	}
	f.Pushes = append(f.Pushes, p)
	return true
}

func factsEqual(a, b *core.PullRequestFacts) bool {
	eqT := func(x, y *time.Time) bool { return (x == nil) == (y == nil) && (x == nil || x.Equal(*y)) }
	eqI := func(x, y *int) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.URL == b.URL && a.HeadBranch == b.HeadBranch && a.Opener == b.Opener && a.OpenerKind == b.OpenerKind &&
		eqT(a.OpenedAt, b.OpenedAt) && eqT(a.FirstReviewAt, b.FirstReviewAt) && eqT(a.MergedAt, b.MergedAt) &&
		eqT(a.ClosedAt, b.ClosedAt) && eqI(a.Additions, b.Additions) && eqI(a.Deletions, b.Deletions) && len(a.Pushes) == len(b.Pushes)
}

func isNotFound(err error) bool { return errors.Is(err, core.ErrNotFound) }
