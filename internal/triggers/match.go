package triggers

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Match reports whether ev starts automation a: the automation is enabled,
// belongs to the event's factory (when the event is factory-scoped), and at
// least one of its triggers matches. Evaluation errors fail closed (the
// trigger does not match) and are logged at warn level.
func Match(ev *core.Event, a core.Automation) bool {
	if ev == nil || !a.Enabled {
		return false
	}
	if ev.FactoryID != "" && a.FactoryID != "" && ev.FactoryID != a.FactoryID {
		return false
	}
	for i, t := range a.Triggers {
		if canonicalProvider(t.Provider) == ProviderSchedule {
			// Schedule events are addressed to one automation.
			if id, _ := ev.Attributes[AttrAutomationID].(string); id != a.ID {
				continue
			}
		}
		ok, err := MatchTrigger(ev, t)
		if err != nil {
			slog.Warn("trigger evaluation failed; not matching",
				"automation_id", a.ID, "automation", a.Name, "trigger", i,
				"provider", t.Provider, "event", t.Event, "err", err)
			continue
		}
		if ok {
			return true
		}
	}
	return false
}

// MatchingAutomations returns the automations in list that ev starts.
func MatchingAutomations(ev *core.Event, list []core.Automation) []core.Automation {
	var out []core.Automation
	for _, a := range list {
		if Match(ev, a) {
			out = append(out, a)
		}
	}
	return out
}

// MatchTrigger evaluates one trigger against an event: provider, event type,
// every filter key (AND; list = any-of; not_in excludes), and provider
// defaults (GitHub skip_bots=all, commit_status only for success/failure/error,
// self-authored activity ignored for chat/tracker/GitLab/Azure DevOps
// providers). A malformed filter returns an error and must be treated as no
// match.
//
// For schedule triggers only the schedule name is compared; use Match to also
// bind the event to its automation.
func MatchTrigger(ev *core.Event, t core.Trigger) (bool, error) {
	if ev == nil {
		return false, errors.New("nil event")
	}
	provider := canonicalProvider(t.Provider)
	if canonicalProvider(ev.Provider) != provider {
		return false, nil
	}
	if !eventTypeMatches(provider, t.Event, ev.Type) {
		return false, nil
	}
	pf, err := parseFilter(provider, t.Event, t.Filter, modeMatch)
	if err != nil {
		return false, err
	}
	attrs := ev.Attributes
	if attrs == nil {
		attrs = map[string]any{}
	}

	// Provider defaults that apply before filters.
	isSelf := attrBool(attrs, AttrActorIsSelf)
	isBot := ev.Actor.IsBot || attrBool(attrs, AttrActorIsBot) || isSelf
	if selfIgnoredProviders[provider] && isSelf {
		return false, nil
	}
	switch provider {
	case ProviderGitHub:
		switch pf.skipBots {
		case SkipBotsAll:
			if isBot {
				return false, nil
			}
		case SkipBotsSelf:
			if isSelf {
				return false, nil
			}
		}
		if t.Event == "commit_status" {
			st, _ := attrs[AttrCommitState].(string)
			switch strings.ToLower(st) {
			case "success", "failure", "error":
			default:
				return false, nil
			}
		}
		if githubCIEvents[t.Event] {
			for _, c := range pf.conds {
				if c.key == "labels" || c.key == "authors" {
					n, _, err := collect(attrs, []string{AttrLinkedPRCount}, cmpInt)
					if err != nil {
						return false, err
					}
					if len(n) != 1 || n[0] != "1" {
						return false, nil
					}
					break
				}
			}
		}
	case ProviderSchedule:
		want := ""
		if t.Schedule != nil {
			want = t.Schedule.Name
		}
		if got, _ := attrs[AttrScheduleName].(string); got != want {
			return false, nil
		}
	}

	text := ev.Text
	if text == "" {
		text, _ = attrs[AttrText].(string)
	}
	for _, c := range pf.conds {
		ok, err := evalCondition(attrs, text, c)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}

	if pf.payload != nil {
		ok, err := MatchPayload(pf.payload, ev.Payload)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// eventTypeMatches compares a trigger's event with the event's type. Slack
// "message_dm" covers both one-to-one (message_im) and group (message_mpim)
// direct messages.
func eventTypeMatches(provider, trigger, got string) bool {
	if trigger == got {
		return true
	}
	if provider == ProviderSlack && trigger == "message_dm" {
		return got == "message_im" || got == "message_mpim"
	}
	return false
}

// canonicalProvider maps integration identifiers to trigger provider names
// (e.g. "microsoft-teams" / "microsoft_teams" -> "teams",
// "azure-devops" -> "azure_devops").
func canonicalProvider(p string) string {
	p = strings.ToLower(strings.TrimSpace(p))
	p = strings.ReplaceAll(p, "-", "_")
	switch p {
	case "microsoft_teams", "msteams":
		return ProviderTeams
	case "azuredevops", "ado":
		return ProviderAzureDevOps
	}
	return p
}

// ValidateTrigger checks an authored trigger (definition file or API) against
// the catalog: known provider/event, accepted filter keys and aliases,
// matcher shapes, value types, in/not_in conflicts, required keys, payload
// pattern rules and, for schedules, the cron expression. Provider identifiers
// must be canonical (e.g. "teams", not "microsoft-teams").
func ValidateTrigger(t core.Trigger) error {
	if t.Provider == "" {
		return filterErr("provider is required")
	}
	if _, ok := catalog[t.Provider]; !ok {
		if c := canonicalProvider(t.Provider); c != t.Provider {
			if _, ok := catalog[c]; ok {
				return filterErr("unknown provider %q; use %q", t.Provider, c)
			}
		}
		return filterErr("unknown provider %q", t.Provider)
	}
	if t.Event == "" {
		return filterErr("event is required")
	}
	if strings.Contains(t.Event, ".") {
		return filterErr("event %q must not be dotted; set provider and event separately", t.Event)
	}
	if t.Provider == ProviderSchedule {
		if t.Schedule == nil {
			return filterErr("schedule is required for schedule.%s", t.Event)
		}
		if err := ValidateCron(t.Schedule.Cron); err != nil {
			return fmt.Errorf("%w: schedule.cron: %w", ErrInvalidFilter, err)
		}
	} else if t.Schedule != nil {
		return filterErr("schedule is only valid for schedule triggers")
	}
	_, err := parseFilter(t.Provider, t.Event, t.Filter, modeAuthor)
	return err
}

// ValidateTriggers validates each trigger of one automation plus cross-trigger
// rules: schedule names must be unique and at most one schedule may omit its
// name.
func ValidateTriggers(ts []core.Trigger) error {
	if len(ts) == 0 {
		return filterErr("at least one trigger is required")
	}
	names := map[string]bool{}
	for i, t := range ts {
		if err := ValidateTrigger(t); err != nil {
			return fmt.Errorf("triggers[%d]: %w", i, err)
		}
		if t.Schedule == nil {
			continue
		}
		if names[t.Schedule.Name] {
			if t.Schedule.Name == "" {
				return filterErr("triggers[%d]: at most one schedule can omit its name", i)
			}
			return filterErr("triggers[%d]: duplicate schedule name %q", i, t.Schedule.Name)
		}
		names[t.Schedule.Name] = true
	}
	return nil
}
