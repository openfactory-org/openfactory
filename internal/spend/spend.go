// Package spend compares the model spend of a factory and of its team with
// the spend limits.
//
// Limits:
//
//   - A factory limit is spendLimit.daily and spendLimit.monthly of
//     factory.yaml.
//   - A team limit is a server setting (OF_TEAM_SPEND_LIMIT_DAILY and
//     OF_TEAM_SPEND_LIMIT_MONTHLY). It applies to each team of the server.
//
// Periods are calendar days and calendar months in UTC. The spend of a
// period is the sum of usage.cost_usd of the runs created in that period. A
// run that continues into the next period keeps its cost in the period in
// which it was created.
//
// The dispatcher uses a Tracker before each claim (a run over a limit waits
// in queued with the reason spend_limit) and at each turn end (the run tree
// stops with spend_limit_exceeded). When 80 percent of a limit is used, each
// admin of the team gets one inbox item for that period.
package spend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/store/storeutil"
)

// WarnFraction is the part of a limit at which admins get an inbox item.
const WarnFraction = 0.8

// Scope tells whose spend a limit counts.
type Scope string

// Scopes.
const (
	ScopeFactory Scope = "factory"
	ScopeTeam    Scope = "team"
)

// Period is the length of a limit.
type Period string

// Periods.
const (
	PeriodDaily   Period = "daily"
	PeriodMonthly Period = "monthly"
)

// Limit is one limit with its spend.
type Limit struct {
	Scope    Scope     `json:"scope"`
	Period   Period    `json:"period"`
	SpentUSD float64   `json:"spent_usd"`
	LimitUSD float64   `json:"limit_usd"`
	Start    time.Time `json:"period_start"`
	ResetsAt time.Time `json:"resets_at"`
}

// Over reports whether the spend reached the limit.
func (l Limit) Over() bool { return l.LimitUSD > 0 && l.SpentUSD >= l.LimitUSD }

// Fraction is the used part of the limit (1 = the limit).
func (l Limit) Fraction() float64 {
	if l.LimitUSD <= 0 {
		return 0
	}
	return l.SpentUSD / l.LimitUSD
}

// Report is the spend of one factory and its team, with each limit that is set.
type Report struct {
	TeamID    string `json:"team_id"`
	FactoryID string `json:"factory_id,omitempty"`
	// Spend of the factory and of the team in the current day and month,
	// in US dollars. The team spend is computed only when a team limit is set.
	FactoryDailyUSD   float64 `json:"factory_daily_usd"`
	FactoryMonthlyUSD float64 `json:"factory_monthly_usd"`
	TeamDailyUSD      float64 `json:"team_daily_usd,omitempty"`
	TeamMonthlyUSD    float64 `json:"team_monthly_usd,omitempty"`
	// Limits lists the limits that are set, with their spend.
	Limits []Limit `json:"limits"`
}

// Exceeded returns the first limit that the spend reached, or nil.
func (r *Report) Exceeded() *Limit {
	if r == nil {
		return nil
	}
	for i := range r.Limits {
		if r.Limits[i].Over() {
			return &r.Limits[i]
		}
	}
	return nil
}

// Warnings returns the limits of which at least WarnFraction is used.
func (r *Report) Warnings() []Limit {
	if r == nil {
		return nil
	}
	var out []Limit
	for _, l := range r.Limits {
		if l.LimitUSD > 0 && l.Fraction() >= WarnFraction {
			out = append(out, l)
		}
	}
	return out
}

// Options configures a Tracker.
type Options struct {
	Store core.Store
	// TeamLimit is the server setting for each team. Zero fields mean no limit.
	TeamLimit core.SpendLimit
	// FactoryLimit returns spendLimit of the active definition of a factory.
	// Nil, or a nil result, means no factory limit.
	FactoryLimit func(ctx context.Context, factoryID string) (*core.SpendLimit, error)
	// OnInbox is called after an inbox item is stored. It can be nil.
	OnInbox func(ctx context.Context, item *core.InboxItem)
	Now     func() time.Time
	Logger  *slog.Logger
}

// Tracker computes spend reports and sends the 80 percent inbox items.
type Tracker struct {
	opts Options
	log  *slog.Logger
}

// New returns a Tracker.
func New(opts Options) *Tracker {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Tracker{opts: opts, log: opts.Logger.With("component", "spend")}
}

// TeamLimit returns the server limit for each team.
func (t *Tracker) TeamLimit() core.SpendLimit { return t.opts.TeamLimit }

func dayStart(now time.Time) time.Time {
	y, m, d := now.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func monthStart(now time.Time) time.Time {
	y, m, _ := now.UTC().Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// Report returns the spend and the limits of a factory and its team. A
// factory ID "" reports the team only.
func (t *Tracker) Report(ctx context.Context, teamID, factoryID string) (*Report, error) {
	now := t.opts.Now().UTC()
	day, month := dayStart(now), monthStart(now)
	rep := &Report{TeamID: teamID, FactoryID: factoryID, Limits: []Limit{}}

	var flim *core.SpendLimit
	if factoryID != "" && t.opts.FactoryLimit != nil {
		var err error
		if flim, err = t.opts.FactoryLimit(ctx, factoryID); err != nil {
			return nil, fmt.Errorf("spend limit of factory %s: %w", factoryID, err)
		}
	}
	if factoryID != "" {
		d, m, err := t.sum(ctx, core.RunFilter{FactoryID: factoryID}, day, month)
		if err != nil {
			return nil, err
		}
		rep.FactoryDailyUSD, rep.FactoryMonthlyUSD = d, m
	}
	tl := t.opts.TeamLimit
	if teamID != "" && (tl.DailyUSD > 0 || tl.MonthlyUSD > 0) {
		d, m, err := t.sum(ctx, core.RunFilter{TeamID: teamID}, day, month)
		if err != nil {
			return nil, err
		}
		rep.TeamDailyUSD, rep.TeamMonthlyUSD = d, m
	}
	add := func(scope Scope, p Period, spent, limit float64) {
		if limit <= 0 {
			return
		}
		l := Limit{Scope: scope, Period: p, SpentUSD: spent, LimitUSD: limit, Start: day, ResetsAt: day.AddDate(0, 0, 1)}
		if p == PeriodMonthly {
			l.Start, l.ResetsAt = month, month.AddDate(0, 1, 0)
		}
		rep.Limits = append(rep.Limits, l)
	}
	if flim != nil {
		add(ScopeFactory, PeriodDaily, rep.FactoryDailyUSD, flim.DailyUSD)
		add(ScopeFactory, PeriodMonthly, rep.FactoryMonthlyUSD, flim.MonthlyUSD)
	}
	add(ScopeTeam, PeriodDaily, rep.TeamDailyUSD, tl.DailyUSD)
	add(ScopeTeam, PeriodMonthly, rep.TeamMonthlyUSD, tl.MonthlyUSD)
	return rep, nil
}

// sum adds the cost of the runs that match f and were created since month,
// and of those created since day.
func (t *Tracker) sum(ctx context.Context, f core.RunFilter, day, month time.Time) (daily, monthly float64, err error) {
	f.Since = &month
	f.ListOpts = core.ListOpts{Limit: storeutil.MaxLimit}
	for {
		runs, err := t.opts.Store.ListRuns(ctx, f)
		if err != nil {
			return 0, 0, fmt.Errorf("list runs for spend: %w", err)
		}
		for _, r := range runs {
			if r.CreatedAt.Before(month) {
				continue
			}
			monthly += r.Usage.CostUSD
			if !r.CreatedAt.Before(day) {
				daily += r.Usage.CostUSD
			}
		}
		if len(runs) < f.Limit {
			return daily, monthly, nil
		}
		last := runs[len(runs)-1]
		f.Cursor = storeutil.EncodeCursor(last.CreatedAt, last.ID)
	}
}

// Configured reports whether any limit can apply to the factory.
func (t *Tracker) Configured(ctx context.Context, factoryID string) bool {
	if tl := t.opts.TeamLimit; tl.DailyUSD > 0 || tl.MonthlyUSD > 0 {
		return true
	}
	if factoryID == "" || t.opts.FactoryLimit == nil {
		return false
	}
	l, err := t.opts.FactoryLimit(ctx, factoryID)
	return err != nil || (l != nil && (l.DailyUSD > 0 || l.MonthlyUSD > 0))
}

// Exceeded returns the limit that the spend of the run's factory or team
// reached, or nil. It sends the 80 percent inbox items on the way.
func (t *Tracker) Exceeded(ctx context.Context, run *core.Run) (*Limit, error) {
	if run == nil || !t.Configured(ctx, run.FactoryID) {
		return nil, nil
	}
	rep, err := t.Report(ctx, run.TeamID, run.FactoryID)
	if err != nil {
		return nil, err
	}
	t.Warn(ctx, rep)
	return rep.Exceeded(), nil
}

// warnKey is the KV key that records the inbox items of one limit and period.
func warnKey(rep *Report, l Limit) string {
	id := rep.FactoryID
	if l.Scope == ScopeTeam {
		id = rep.TeamID
	}
	return fmt.Sprintf("spend:warned:%s:%s:%s:%s", l.Scope, id, l.Period, l.Start.Format("2006-01-02"))
}

// Warn creates one inbox item for each admin of the team, the first time in
// a period that WarnFraction of a limit is used. The item goes to the inbox
// of the report's factory. Replicas that check at the same time can create
// a second item; the KV marker keeps this rare.
func (t *Tracker) Warn(ctx context.Context, rep *Report) {
	if rep == nil || rep.FactoryID == "" {
		return
	}
	for _, l := range rep.Warnings() {
		key := warnKey(rep, l)
		if v, err := t.opts.Store.KVGet(ctx, key); err == nil && v != "" {
			continue
		} else if err != nil && !errors.Is(err, core.ErrNotFound) {
			t.log.Warn("read spend marker", "key", key, "err", err)
			continue
		}
		if err := t.opts.Store.KVSet(ctx, key, t.opts.Now().UTC().Format(time.RFC3339)); err != nil {
			t.log.Warn("write spend marker", "key", key, "err", err)
			continue
		}
		t.notifyAdmins(ctx, rep, l)
	}
}

func (t *Tracker) notifyAdmins(ctx context.Context, rep *Report, l Limit) {
	users, err := t.opts.Store.ListUsers(ctx, rep.TeamID)
	if err != nil {
		t.log.Warn("list admins for spend notice", "team", rep.TeamID, "err", err)
		return
	}
	var admins []string
	for _, u := range users {
		if core.NormalizeRole(u.Role) == core.RoleAdmin && u.Kind != core.UserKindServiceAccount {
			admins = append(admins, u.ID)
		}
	}
	if len(admins) == 0 {
		admins = []string{""} // an item for the team, without an assignee
	}
	title, body := noticeText(l)
	for _, id := range admins {
		item := &core.InboxItem{
			ID:         core.NewID("inb"),
			FactoryID:  rep.FactoryID,
			Kind:       core.InboxSpendLimit,
			Title:      title,
			Body:       body,
			AssigneeID: id,
			CreatedAt:  t.opts.Now().UTC(),
		}
		if err := t.opts.Store.CreateInboxItem(ctx, item); err != nil {
			t.log.Warn("create spend notice", "factory", rep.FactoryID, "err", err)
			continue
		}
		if t.opts.OnInbox != nil {
			t.opts.OnInbox(ctx, item)
		}
	}
	t.log.Info("spend limit notice", "scope", l.Scope, "period", l.Period, "team", rep.TeamID, "factory", rep.FactoryID,
		"spent_usd", l.SpentUSD, "limit_usd", l.LimitUSD)
}

func noticeText(l Limit) (title, body string) {
	who := "This factory"
	where := "spendLimit." + string(l.Period) + " in factory.yaml"
	if l.Scope == ScopeTeam {
		who = "The team"
		where = "OF_TEAM_SPEND_LIMIT_" + strings.ToUpper(string(l.Period)) + " on the server"
	}
	pct := int(l.Fraction()*100 + 0.5)
	title = fmt.Sprintf("%s used %d%% of its %s spend limit", who, pct, l.Period)
	body = fmt.Sprintf("%s spent $%.2f of its %s limit of $%.2f (%s). "+
		"When the limit is reached, new runs wait in queued and running runs stop at their next turn end. "+
		"The limit resets at %s. To change it, edit %s.",
		who, l.SpentUSD, l.Period, l.LimitUSD, periodLabel(l), l.ResetsAt.Format("2006-01-02 15:04 MST"), where)
	return title, body
}

func periodLabel(l Limit) string {
	if l.Period == PeriodMonthly {
		return l.Start.Format("January 2006") + ", UTC"
	}
	return l.Start.Format("2006-01-02") + ", UTC"
}
