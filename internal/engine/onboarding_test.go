package engine

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/openfactory-org/openfactory/internal/core"
)

type commentForge struct {
	core.Forge
	mu       sync.Mutex
	comments []string
}

func (f *commentForge) Name() string { return "GITHUB" }
func (f *commentForge) Comment(_ context.Context, owner, repo string, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, owner+"/"+repo+"#"+itoa(number)+": "+body)
	return nil
}

type commentForges struct{ f *commentForge }

func (c commentForges) Forge(context.Context, core.ForgeLookup) (core.Forge, error) { return c.f, nil }

func itoa(n int) string { return strconv.Itoa(n) }

// mentionEvent is a GitHub comment that mentions the factory alias.
func mentionEvent(actor string) *core.Event {
	ev := IssueEvent("issue_mentioned", "@payments please look", actor)
	ev.Attributes["alias"] = "payments"
	return ev
}

// TestGitHubOnboardingNotices is a regression test for
// automations.githubOnboardingNotices: a GitHub mention from an unlinked or
// non-member account does not start work; with notices on (the default) the
// account gets one onboarding comment, with notices off it gets none. The
// check applies to mentions only: an automation match runs as the agent.
func TestGitHubOnboardingNotices(t *testing.T) {
	ctx := context.Background()
	unlinked := func() *core.Event {
		ev := mentionEvent("stranger")
		ev.Actor.UserID = ""
		return ev
	}
	tests := []struct {
		name     string
		notices  bool
		events   []*core.Event
		comments int
		want     string
	}{
		// The comment is public: it names the dashboard and holds no link URL.
		{"unlinked, notices on, once", true, []*core.Event{unlinked(), unlinked()}, 1, "Link your account"},
		{"non-member", true, []*core.Event{func() *core.Event {
			ev := mentionEvent("outsider")
			ev.Actor.UserID = "ext_user"
			return ev
		}()}, 1, "not an active member"},
		{"notices off", false, []*core.Event{unlinked()}, 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := NewTestEnv(t, func(d *core.CompiledDefinition) { d.GithubOnboardingNotices = tc.notices })
			env.Store.PutUser(&core.User{ID: "ext_user", TeamID: "other-team"})
			cf := &commentForge{}
			env.Engine.forges = commentForges{cf}
			env.Engine.SetLinkURL(func(_ string, a core.Actor) string { return "https://cp.test/ui/link/" + a.Handle })
			for i, ev := range tc.events {
				ev.DeliveryID = tc.name + itoa(i)
				runs, err := env.Engine.HandleEvent(ctx, ev)
				if err != nil || len(runs) != 0 {
					t.Fatalf("runs = %v, err = %v (ineligible accounts must not start work)", runs, err)
				}
			}
			if len(cf.comments) != tc.comments {
				t.Fatalf("comments = %v", cf.comments)
			}
			if tc.want != "" && (!strings.Contains(cf.comments[0], tc.want) || !strings.HasPrefix(cf.comments[0], "acme/api#7: ") || strings.Contains(cf.comments[0], "/ui/link/")) {
				t.Fatalf("comment = %q", cf.comments[0])
			}
		})
	}

	// A linked team member still starts work with a mention.
	env := NewTestEnv(t)
	runs, err := env.Engine.HandleEvent(ctx, mentionEvent("maintainer"))
	if err != nil || len(runs) != 1 || runs[0].TriggeredBy != "mention" {
		t.Fatalf("member runs = %v, err = %v", runs, err)
	}
	// An automation match from an unlinked account starts its run.
	out := IssueEvent("issue_labeled", "outside report", "contributor")
	out.Actor.UserID = ""
	out.Source.Number = 8
	runs, err = env.Engine.HandleEvent(ctx, out)
	if err != nil || len(runs) != 1 || !strings.HasPrefix(runs[0].TriggeredBy, "automation:") {
		t.Fatalf("automation runs = %v, err = %v", runs, err)
	}
	// Bot / CI events are not gated (automation bot policies still apply).
	bot := IssueEvent("check_run_completed", "", "ci-bot")
	bot.Actor.UserID, bot.Actor.IsBot = "", true
	if !env.Engine.githubActorEligible(ctx, bot, []core.Factory{*env.Factory}) {
		t.Fatal("bot event gated")
	}
}
