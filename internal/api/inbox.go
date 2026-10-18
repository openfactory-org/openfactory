package api

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// InboxItemView is the API representation of an inbox item. It shows
// recipients and the read state of the caller. The
// stored read list and the old assignee are not in the response.
type InboxItemView struct {
	core.InboxItem
	// Recipients are the users that the item is for. An empty list means
	// the editors and admins of the factory.
	Recipients []string `json:"recipients"`
	// IsRead is the read state for the caller.
	IsRead bool `json:"is_read"`
	// The fields below hide stored fields of the embedded item. They are
	// always empty, so they are not in the response.
	ReadBy          []string `json:"read_by,omitempty"`
	AssigneeID      string   `json:"assignee_id,omitempty"`
	AnswerMessageID string   `json:"answer_message_id,omitempty"`
}

func inboxView(it *core.InboxItem, p *Principal) InboxItemView {
	rc := it.RecipientIDs()
	if rc == nil {
		rc = []string{}
	}
	return InboxItemView{InboxItem: *it, Recipients: rc, IsRead: it.IsReadBy(p.UserID())}
}

func inboxViews(items []core.InboxItem, p *Principal) []InboxItemView {
	out := make([]InboxItemView, len(items))
	for i := range items {
		out[i] = inboxView(&items[i], p)
	}
	return out
}

// Inbox scopes of GET /inbox.
const (
	inboxScopeMine = "mine"
	inboxScopeTeam = "team"
)

type inboxFilter struct {
	kinds           []string
	includeResolved bool
	scope           string
	unread          bool
	search          string
}

func inboxFilterFrom(r *http.Request) (inboxFilter, error) {
	var f inboxFilter
	var err error
	if f.includeResolved, err = qBool(r, "include_resolved"); err != nil {
		return f, err
	}
	if f.unread, err = qBool(r, "unread"); err != nil {
		return f, err
	}
	switch sc := strings.ToLower(r.URL.Query().Get("scope")); sc {
	case "", inboxScopeMine:
		f.scope = inboxScopeMine
	case inboxScopeTeam:
		f.scope = inboxScopeTeam
	default:
		return f, errInvalid("unknown scope %q (expected mine or team)", sc)
	}
	for _, k := range qList(r, "kind") {
		switch core.InboxKind(k) {
		case core.InboxApproval, core.InboxQuestion, core.InboxHandoff, core.InboxReview,
			core.InboxFailure, core.InboxBlocked, core.InboxAnswered, core.InboxSpendLimit:
		default:
			return f, errInvalid("unknown kind %q (expected question, approval, handoff, review, failure, blocked, answered or spend_limit)", k)
		}
		f.kinds = append(f.kinds, k)
	}
	f.search = strings.ToLower(r.URL.Query().Get("search"))
	return f, nil
}

// canSeeTeamInbox tells that the caller is an editor or an admin of the
// factory. Only they list scope=team and get the items without recipients.
func canSeeTeamInbox(p *Principal, factoryID string) bool {
	return core.RoleRank(p.RoleFor(factoryID)) >= core.RoleRank(core.RoleEditor)
}

// forCaller tells that an item needs the caller: the caller is a recipient,
// or the item has no recipients and the caller is an editor or an admin of
// the factory.
func forCaller(it *core.InboxItem, p *Principal) bool {
	rc := it.RecipientIDs()
	if len(rc) == 0 {
		return canSeeTeamInbox(p, it.FactoryID)
	}
	return p.UserID() != "" && slices.Contains(rc, p.UserID())
}

// InboxForCaller tells that an item needs the caller. The Factory MCP
// tools use it, so that they show the personal queue by default.
func InboxForCaller(it *core.InboxItem, p *Principal) bool { return forCaller(it, p) }

func filterInbox(p *Principal, items []core.InboxItem, f inboxFilter) []core.InboxItem {
	return slices.DeleteFunc(items, func(it core.InboxItem) bool {
		if len(f.kinds) > 0 && !slices.Contains(f.kinds, string(it.Kind)) {
			return true
		}
		if f.search != "" && !strings.Contains(strings.ToLower(it.Title+" "+it.Body), f.search) {
			return true
		}
		if f.unread && it.IsReadBy(p.UserID()) {
			return true
		}
		if f.scope == inboxScopeMine {
			return !forCaller(&it, p)
		}
		return false
	})
}

func (s *Server) handleListInbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	return s.listInbox(w, r, p, []core.Factory{*f})
}

func (s *Server) handleListTeamInbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var fs []core.Factory
	if fid := r.URL.Query().Get("factory_id"); fid != "" {
		f, err := s.factoryFor(ctx, p, fid)
		if err != nil {
			return err
		}
		fs = []core.Factory{*f}
	} else {
		var err error
		if fs, err = s.opts.Store.ListFactories(ctx, p.TeamID, ""); err != nil {
			return err
		}
	}
	return s.listInbox(w, r, p, fs)
}

func (s *Server) listInbox(w http.ResponseWriter, r *http.Request, p *Principal, fs []core.Factory) error {
	ctx := r.Context()
	pg, err := pageFrom(r)
	if err != nil {
		return err
	}
	flt, err := inboxFilterFrom(r)
	if err != nil {
		return err
	}
	if flt.scope == inboxScopeTeam {
		// scope=team lists the factories where the caller is an editor or
		// an admin. A single named factory without that role is an error.
		allowed := fs[:0:0]
		for _, f := range fs {
			if canSeeTeamInbox(p, f.ID) {
				allowed = append(allowed, f)
			}
		}
		if len(allowed) == 0 && len(fs) > 0 {
			return errForbidden("scope=team needs the editor or admin role")
		}
		fs = allowed
	}
	var all []core.InboxItem
	for _, f := range fs {
		items, err := s.opts.Store.ListInboxItems(ctx, f.ID, flt.includeResolved)
		if err != nil {
			return err
		}
		all = append(all, items...)
	}
	all = filterInbox(p, all, flt)
	page, next := paginate(all, func(it core.InboxItem) (timeT, string) { return it.CreatedAt, it.ID }, pg)
	writeJSON(w, http.StatusOK, listResponse("inbox_items", inboxViews(page, p), next))
	return nil
}

func (s *Server) inboxFor(ctx context.Context, p *Principal, id string) (*core.InboxItem, error) {
	it, err := s.opts.Store.GetInboxItem(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound("inbox item " + id)
		}
		return nil, err
	}
	if _, err := s.factoryFor(ctx, p, it.FactoryID); err != nil {
		return nil, errNotFound("inbox item " + id)
	}
	return it, nil
}

func (s *Server) handleGetInboxItem(w http.ResponseWriter, r *http.Request, p *Principal) error {
	it, err := s.inboxFor(r.Context(), p, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, inboxView(it, p))
	return nil
}

// handleReadInbox marks an item as read (or unread with {"read": false})
// for the caller.
func (s *Server) handleReadInbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Read *bool `json:"read"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	read := req.Read == nil || *req.Read
	uid := p.UserID()
	if uid == "" {
		return errInvalid("the read state needs a user or a service account as the caller")
	}
	it, err := s.inboxFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		if it.IsReadBy(uid) == read {
			break
		}
		if read {
			it.ReadBy = append(it.ReadBy, uid)
		} else {
			it.ReadBy = slices.DeleteFunc(it.ReadBy, func(u string) bool { return u == uid })
		}
		err := s.opts.Store.UpdateInboxItem(ctx, it)
		if err == nil {
			break
		}
		if !errorsIs(err, core.ErrStale) || attempt >= 3 {
			return err
		}
		if it, err = s.opts.Store.GetInboxItem(ctx, it.ID); err != nil {
			return err
		}
	}
	writeJSON(w, http.StatusOK, inboxView(it, p))
	return nil
}

// InboxAnswer is the answer to one question of an inbox item.
type InboxAnswer struct {
	QuestionID string   `json:"question_id"`
	Selected   []string `json:"selected,omitempty"`
	Text       string   `json:"text,omitempty"`
}

// answersResolution checks the answers against the questions of an item and
// writes them as the text of the resolution.
func answersResolution(it *core.InboxItem, answers []InboxAnswer) (string, error) {
	var b strings.Builder
	for _, a := range answers {
		qi := slices.IndexFunc(it.Questions, func(q core.Question) bool { return q.ID == a.QuestionID })
		if qi < 0 {
			return "", errInvalid("unknown question_id %q", a.QuestionID)
		}
		q := it.Questions[qi]
		var parts []string
		for _, sel := range a.Selected {
			oi := slices.IndexFunc(q.Options, func(o string) bool { return strings.EqualFold(o, strings.TrimSpace(sel)) })
			if oi < 0 {
				return "", errInvalid("answer %q to question %s must be one of: %s", sel, q.ID, strings.Join(q.Options, ", "))
			}
			parts = append(parts, q.Options[oi])
		}
		if q.Type == core.QuestionSingleSelect && len(parts) > 1 {
			return "", errInvalid("question %s takes one choice", q.ID)
		}
		if t := strings.TrimSpace(a.Text); t != "" {
			parts = append(parts, t)
		}
		if len(parts) == 0 {
			return "", errInvalid("answer to question %s is empty", q.ID)
		}
		if len(it.Questions) > 1 {
			b.WriteString(q.Text + ": ")
		}
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String()), nil
}

func (s *Server) handleResolveInbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	var req struct {
		Resolution string        `json:"resolution"`
		Answers    []InboxAnswer `json:"answers"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	it, err := s.inboxFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if it.Resolved {
		return Errf(CodeConflict, "inbox item is already resolved")
	}
	res := strings.TrimSpace(req.Resolution)
	if len(req.Answers) > 0 {
		if res != "" {
			return errInvalid("send resolution or answers, not both")
		}
		if res, err = answersResolution(it, req.Answers); err != nil {
			return err
		}
	}
	if handled, err := s.resolveProposalInboxItem(ctx, p, it, res); handled {
		if err != nil {
			return err
		}
		if nit, err := s.opts.Store.GetInboxItem(ctx, it.ID); err == nil {
			it = nit
		}
		writeJSON(w, http.StatusOK, inboxView(it, p))
		return nil
	} else if err != nil {
		return err
	}
	switch it.Kind {
	case core.InboxApproval:
		if res == "" {
			return errInvalid("resolution is required for approvals")
		}
		if len(it.Options) > 0 {
			idx := slices.IndexFunc(it.Options, func(o string) bool { return strings.EqualFold(o, res) })
			if idx < 0 {
				return errInvalid("resolution must be one of: %s", strings.Join(it.Options, ", "))
			}
			res = it.Options[idx]
		}
	case core.InboxQuestion, core.InboxBlocked:
		if res == "" {
			return errInvalid("resolution (the answer) is required for a %s item", it.Kind)
		}
	default:
		if res == "" {
			res = "resolved"
		}
	}
	ctx = core.WithResolver(ctx, core.Resolver{UserID: p.UserID(), Via: resolvedVia(p)})
	if err := s.opts.Engine.ResolveInbox(ctx, it.ID, res, p.ActorID()); err != nil {
		return err
	}
	if nit, err := s.opts.Store.GetInboxItem(ctx, it.ID); err == nil {
		it = nit
	}
	writeJSON(w, http.StatusOK, inboxView(it, p))
	return nil
}

// handleUnresolveInbox undoes a resolve. The engine allows it for 10
// minutes, and only while the run did not consume the answer.
func (s *Server) handleUnresolveInbox(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	it, err := s.inboxFor(ctx, p, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !it.Resolved {
		return Errf(CodeConflict, "inbox item is not resolved")
	}
	nit, err := s.opts.Engine.UnresolveInbox(ctx, it.ID, p.ActorID())
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, inboxView(nit, p))
	return nil
}
