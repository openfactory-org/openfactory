package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// DefinitionProposals reads and decides proposed definition versions.
// scoring.Service implements it.
type DefinitionProposals interface {
	ListProposals(ctx context.Context, factoryID string, all bool) ([]core.DefinitionVersion, error)
	DiffProposal(ctx context.Context, v *core.DefinitionVersion) (string, error)
	ApproveProposal(ctx context.Context, factoryID, id, actor string) (*core.DefinitionVersion, error)
	RejectProposal(ctx context.Context, factoryID, id, actor, comment string) (*core.DefinitionVersion, error)
	// ProposalForInboxItem returns the open proposal that the review item
	// belongs to, or nil when the item is not a proposal.
	ProposalForInboxItem(ctx context.Context, factoryID, inboxID string) (*core.DefinitionVersion, error)
}

// ProposalView is a proposed definition version in the API: the version
// without its files, with the diff against its base version.
type ProposalView struct {
	ID               string                 `json:"id"`
	FactoryID        string                 `json:"factory_id"`
	State            string                 `json:"state"`
	BaseVersionID    string                 `json:"base_version_id"`
	AuthorRunID      string                 `json:"author_run_id,omitempty"`
	InboxItemID      string                 `json:"inbox_item_id,omitempty"`
	Message          string                 `json:"message"`
	Diff             string                 `json:"diff"`
	AppliedVersionID string                 `json:"applied_version_id,omitempty"`
	ReviewedBy       string                 `json:"reviewed_by,omitempty"`
	ReviewComment    string                 `json:"review_comment,omitempty"`
	ReviewedAt       *timeT                 `json:"reviewed_at,omitempty"`
	CreatedAt        timeT                  `json:"created_at"`
	BaseIsActive     bool                   `json:"base_is_active"`
	Errors           []core.ValidationError `json:"errors,omitempty"`
}

func (s *Server) proposalView(ctx context.Context, f *core.Factory, v *core.DefinitionVersion) (ProposalView, error) {
	diff, err := s.opts.Proposals.DiffProposal(ctx, v)
	if err != nil {
		return ProposalView{}, err
	}
	return ProposalView{
		ID: v.ID, FactoryID: v.FactoryID, State: v.State, BaseVersionID: v.BaseVersionID,
		AuthorRunID: v.AuthorRunID, InboxItemID: v.InboxItemID, Message: v.Message, Diff: diff,
		AppliedVersionID: v.AppliedVersionID, ReviewedBy: v.ReviewedBy, ReviewComment: v.ReviewComment,
		ReviewedAt: v.ReviewedAt, CreatedAt: v.CreatedAt, BaseIsActive: v.BaseVersionID == f.ActiveVersionID,
		Errors: v.Errors,
	}, nil
}

// GET /factories/{uid}/definition/proposals?state=all
func (s *Server) handleListProposals(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.Proposals == nil {
		return featureUnavailable("definition proposals")
	}
	all := r.URL.Query().Get("state") == "all"
	vs, err := s.opts.Proposals.ListProposals(ctx, f.ID, all)
	if err != nil {
		return err
	}
	out := make([]ProposalView, 0, len(vs))
	for i := range vs {
		pv, err := s.proposalView(ctx, f, &vs[i])
		if err != nil {
			return err
		}
		out = append(out, pv)
	}
	writeJSON(w, http.StatusOK, listResponse("proposals", out, ""))
	return nil
}

// POST /factories/{uid}/definition/proposals/{id}/approve
func (s *Server) handleApproveProposal(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.Proposals == nil {
		return featureUnavailable("definition proposals")
	}
	ctx = core.WithResolver(ctx, core.Resolver{UserID: p.UserID(), Via: core.InboxViaAPI})
	v, err := s.opts.Proposals.ApproveProposal(ctx, f.ID, r.PathValue("id"), p.ActorID())
	if err != nil {
		if isNotFound(err) {
			return errNotFound("proposal " + r.PathValue("id"))
		}
		return err
	}
	if cur, err := s.opts.Store.GetFactory(ctx, f.ID); err == nil {
		f = cur
	}
	pv, err := s.proposalView(ctx, f, v)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pv)
	return nil
}

// POST /factories/{uid}/definition/proposals/{id}/reject  {"comment": "..."}
func (s *Server) handleRejectProposal(w http.ResponseWriter, r *http.Request, p *Principal) error {
	ctx := r.Context()
	f, err := s.factoryFor(ctx, p, r.PathValue("uid"))
	if err != nil {
		return err
	}
	if s.opts.Proposals == nil {
		return featureUnavailable("definition proposals")
	}
	var req struct {
		Comment string `json:"comment"`
	}
	if err := decodeJSON(r, &req, true); err != nil {
		return err
	}
	ctx = core.WithResolver(ctx, core.Resolver{UserID: p.UserID(), Via: core.InboxViaAPI})
	v, err := s.opts.Proposals.RejectProposal(ctx, f.ID, r.PathValue("id"), p.ActorID(), req.Comment)
	if err != nil {
		if isNotFound(err) {
			return errNotFound("proposal " + r.PathValue("id"))
		}
		return err
	}
	pv, err := s.proposalView(ctx, f, v)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, pv)
	return nil
}

// resolveProposalInboxItem decides the proposal of a review item with one
// click: the resolution "approve" applies the proposed version, and
// "reject" or "reject: <reason>" rejects it and sends the reason to the
// agent. It returns false when the item is not a proposal.
func (s *Server) resolveProposalInboxItem(ctx context.Context, p *Principal, it *core.InboxItem, resolution string) (bool, error) {
	if it.Kind != core.InboxReview || s.opts.Proposals == nil {
		return false, nil
	}
	v, err := s.opts.Proposals.ProposalForInboxItem(ctx, it.FactoryID, it.ID)
	if err != nil {
		return false, err
	}
	if v == nil {
		return false, nil
	}
	if !p.CanOn(it.FactoryID, core.RoleEditor) {
		return true, errForbidden("applying a definition change needs the editor role")
	}
	word, rest, _ := strings.Cut(strings.TrimSpace(resolution), ":")
	word = strings.ToLower(strings.TrimSpace(word))
	rctx := core.WithResolver(ctx, core.Resolver{UserID: p.UserID(), Via: resolvedVia(p)})
	switch word {
	case "approve", "approved":
		_, err = s.opts.Proposals.ApproveProposal(rctx, it.FactoryID, v.ID, p.ActorID())
	case "reject", "rejected":
		_, err = s.opts.Proposals.RejectProposal(rctx, it.FactoryID, v.ID, p.ActorID(), strings.TrimSpace(rest))
	default:
		return true, errInvalid("the resolution of a definition change must be approve or reject (reject: <reason>)")
	}
	return true, err
}
