package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/forge"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// ErrUnimplemented is returned for protocol features a plugin or this host does not support.
var ErrUnimplemented = errors.New("not implemented by plugin")

func hasCap(info *pb.PluginInfo, c string) bool {
	if info == nil {
		return false
	}
	for _, x := range info.Capabilities {
		if x == c {
			return true
		}
	}
	return false
}

// ---------- forges ----------

var _ forge.ForgeProvider = (*Host)(nil)

// forgePlugin returns the plugin serving forges for an integration provider.
func (h *Host) forgePlugin(provider string) *instance {
	provider = strings.ToLower(provider)
	for _, in := range h.named() {
		info := in.pluginInfo()
		if !hasCap(info, wire.CapForge) {
			continue
		}
		for _, p := range info.ForgeProviders {
			if strings.ToLower(p) == provider {
				return in
			}
		}
	}
	return nil
}

// HasForge implements forge.ForgeProvider. It stays true while a plugin that
// served the provider is restarting (calls then fail with ErrUnavailable).
func (h *Host) HasForge(provider string) bool { return h.forgePlugin(provider) != nil }

// ForgeFor implements forge.ForgeProvider.
func (h *Host) ForgeFor(ctx context.Context, t forge.ForgeTarget) (core.Forge, error) {
	in := h.forgePlugin(t.Provider)
	if in == nil {
		return nil, fmt.Errorf("plugin: no plugin serves forge provider %q: %w", t.Provider, forge.ErrPluginNotInstalled)
	}
	f := &pluginForge{h: h, in: in, target: &pb.ForgeTarget{
		Provider: t.Provider, IntegrationId: t.IntegrationID, TeamId: t.TeamID, Config: t.Config, Token: t.Token, UserToken: t.UserToken,
	}}
	// Name has no context in core.Forge: resolve it now (it also validates the
	// target, e.g. a missing token).
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.Name(ctx, &pb.ForgeNameRequest{Target: f.target})
		if err == nil {
			f.name = r.Name
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return f, nil
}

// pluginForge implements core.Forge (plus forge.IssueCommenter and
// forge.WorkItemCommenter) by calling a plugin.
type pluginForge struct {
	h      *Host
	in     *instance
	target *pb.ForgeTarget
	name   string
}

var (
	_ core.Forge               = (*pluginForge)(nil)
	_ forge.IssueCommenter     = (*pluginForge)(nil)
	_ forge.WorkItemCommenter  = (*pluginForge)(nil)
	_ forge.WorkItemTracker    = (*pluginForge)(nil)
	_ core.ReviewRequester     = (*pluginForge)(nil)
	_ core.LoginReader         = (*pluginForge)(nil)
	_ core.ReviewContextReader = (*pluginForge)(nil)
)

func (f *pluginForge) call(ctx context.Context, fn func(context.Context, pb.ForgeClient) error) error {
	conn, err := f.in.conn()
	if err != nil {
		return err
	}
	ctx, cancel := f.h.callCtx(ctx)
	defer cancel()
	return wire.FromStatus(f.in.name(), fn(ctx, pb.NewForgeClient(conn)))
}

func (f *pluginForge) Name() string { return f.name }

func (f *pluginForge) CloneURL(ctx context.Context, owner, repo string) (string, error) {
	var out string
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.CloneURL(ctx, &pb.RepoRequest{Target: f.target, Owner: owner, Repo: repo})
		if err == nil {
			out = r.Url
		}
		return err
	})
	return out, err
}

func (f *pluginForge) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	var out string
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.DefaultBranch(ctx, &pb.RepoRequest{Target: f.target, Owner: owner, Repo: repo})
		if err == nil {
			out = r.Branch
		}
		return err
	})
	return out, err
}

func (f *pluginForge) CreatePullRequest(ctx context.Context, in core.PullRequestInput) (*core.PullRequestRef, error) {
	var out *core.PullRequestRef
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.CreatePullRequest(ctx, &pb.CreatePullRequestRequest{Target: f.target, Owner: in.Owner, Repo: in.Repo,
			Head: in.Head, Base: in.Base, Title: in.Title, Body: in.Body, Draft: in.Draft})
		if err == nil {
			out = wire.PRFromPB(r)
		}
		return err
	})
	return out, err
}

func (f *pluginForge) GetPullRequest(ctx context.Context, owner, repo string, number int) (*core.PullRequestRef, error) {
	var out *core.PullRequestRef
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.GetPullRequest(ctx, &pb.GetPullRequestRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number)})
		if err == nil {
			out = wire.PRFromPB(r)
		}
		return err
	})
	return out, err
}

func (f *pluginForge) Comment(ctx context.Context, owner, repo string, number int, body string) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.Comment(ctx, &pb.CommentRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), Body: body})
		return err
	})
}

// CommentIssue implements forge.IssueCommenter; plugins without distinct
// issue comments fall back to Comment.
func (f *pluginForge) CommentIssue(ctx context.Context, owner, repo string, number int, body string) error {
	unimplemented := false
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.CommentIssue(ctx, &pb.CommentRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), Body: body})
		unimplemented = status.Code(err) == codes.Unimplemented
		return err
	})
	if unimplemented {
		return f.Comment(ctx, owner, repo, number, body)
	}
	return err
}

// CommentWorkItem implements forge.WorkItemCommenter.
func (f *pluginForge) CommentWorkItem(ctx context.Context, project string, id int, body string) error {
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.CommentWorkItem(ctx, &pb.CommentWorkItemRequest{Target: f.target, Project: project, Id: int64(id), Body: body})
		return err
	})
	if isUnimplemented(err) {
		return fmt.Errorf("forge %s cannot comment on work items: %w", f.name, ErrUnimplemented)
	}
	return err
}

func (f *pluginForge) SetCommitStatus(ctx context.Context, owner, repo, sha string, st core.CommitStatus) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.SetCommitStatus(ctx, &pb.SetCommitStatusRequest{Target: f.target, Owner: owner, Repo: repo, Sha: sha,
			Status: &pb.CommitStatus{Context: st.Context, State: st.State, Description: st.Description, TargetUrl: st.TargetURL}})
		return err
	})
}

func (f *pluginForge) GetFiles(ctx context.Context, owner, repo, ref, path string) (map[string]string, string, error) {
	var files map[string]string
	var sha string
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.GetFiles(ctx, &pb.GetFilesRequest{Target: f.target, Owner: owner, Repo: repo, Ref: ref, Path: path})
		if err == nil {
			files, sha = r.Files, r.CommitSha
			if files == nil {
				files = map[string]string{}
			}
		}
		return err
	})
	return files, sha, err
}

// ReplyInThread implements core.Forge.
func (f *pluginForge) ReplyInThread(ctx context.Context, owner, repo string, number int, threadRef, body string) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.ReplyInThread(ctx, &pb.ThreadRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), ThreadRef: threadRef, Body: body})
		return err
	})
}

// ResolveThread implements core.Forge.
func (f *pluginForge) ResolveThread(ctx context.Context, owner, repo string, number int, threadRef string) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.ResolveThread(ctx, &pb.ThreadRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), ThreadRef: threadRef})
		return err
	})
}

// SubmitReview implements core.Forge.
func (f *pluginForge) SubmitReview(ctx context.Context, in core.ReviewInput) error {
	req := &pb.SubmitReviewRequest{Target: f.target, Owner: in.Owner, Repo: in.Repo, Number: int64(in.Number), Event: in.Event, Body: in.Body}
	for _, cm := range in.Comments {
		req.Comments = append(req.Comments, &pb.ReviewComment{Path: cm.Path, Line: int64(cm.Line), Side: cm.Side, Body: cm.Body})
	}
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.SubmitReview(ctx, req)
		return err
	})
}

// MarkReady implements core.Forge.
func (f *pluginForge) MarkReady(ctx context.Context, owner, repo string, number int) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.MarkReady(ctx, &pb.GetPullRequestRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number)})
		return err
	})
}

// SetWorkItemState implements forge.WorkItemTracker. A plugin without work
// items gives core.ErrOperationNotSupported.
func (f *pluginForge) SetWorkItemState(ctx context.Context, project string, id int, state string) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.SetWorkItemState(ctx, &pb.SetWorkItemStateRequest{Target: f.target, Project: project, Id: int64(id), State: state})
		return err
	})
}

// AttachPullRequestToWorkItem implements forge.WorkItemTracker.
func (f *pluginForge) AttachPullRequestToWorkItem(ctx context.Context, project string, id int, pr core.PullRequestRef) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.AttachPullRequestToWorkItem(ctx, &pb.AttachPullRequestToWorkItemRequest{Target: f.target, Project: project, Id: int64(id), PullRequest: wire.PRToPB(&pr)})
		return err
	})
}

// RequestReviewers implements core.ReviewRequester. A plugin that cannot
// request a review gives core.ErrOperationNotSupported.
func (f *pluginForge) RequestReviewers(ctx context.Context, owner, repo string, number int, logins []string) error {
	return f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		_, err := c.RequestReviewers(ctx, &pb.RequestReviewersRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), Logins: logins})
		return err
	})
}

// Login implements core.LoginReader.
func (f *pluginForge) Login(ctx context.Context) (string, error) {
	var out string
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.Login(ctx, &pb.LoginRequest{Target: f.target})
		if err == nil {
			out = r.Login
		}
		return err
	})
	return out, err
}

// ReviewThread implements core.ReviewContextReader.
func (f *pluginForge) ReviewThread(ctx context.Context, owner, repo string, number int, threadRef string) (*core.ReviewThread, error) {
	var out *core.ReviewThread
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.GetReviewThread(ctx, &pb.ThreadRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), ThreadRef: threadRef})
		if err == nil {
			out = wire.ReviewThreadFromPB(r)
		}
		return err
	})
	return out, err
}

// FileDiff implements core.ReviewContextReader.
func (f *pluginForge) FileDiff(ctx context.Context, owner, repo string, number int, path string) (*core.FileDiff, error) {
	var out *core.FileDiff
	err := f.call(ctx, func(ctx context.Context, c pb.ForgeClient) error {
		r, err := c.GetFileDiff(ctx, &pb.FileDiffRequest{Target: f.target, Owner: owner, Repo: repo, Number: int64(number), Path: path})
		if err == nil {
			out = &core.FileDiff{Path: r.Path, Patch: r.Patch, Truncated: r.Truncated}
		}
		return err
	})
	return out, err
}

func isUnimplemented(err error) bool {
	var re *wire.RemoteError
	return errors.As(err, &re) && re.Code == codes.Unimplemented
}

// ---------- responders ----------

// Responders returns a core.Responder for each provider a running plugin
// replies to (PluginInfo.responder_providers).
func (h *Host) Responders() []core.Responder {
	var out []core.Responder
	for _, in := range h.named() {
		info := in.pluginInfo()
		if !hasCap(info, wire.CapResponder) {
			continue
		}
		for _, p := range info.ResponderProviders {
			out = append(out, &pluginResponder{h: h, in: in, provider: p})
		}
	}
	return out
}

type pluginResponder struct {
	h        *Host
	in       *instance
	provider string
}

var (
	_ core.Responder    = (*pluginResponder)(nil)
	_ core.IssueTracker = (*pluginResponder)(nil)
)

func (r *pluginResponder) Provider() string { return r.provider }

func (r *pluginResponder) Reply(ctx context.Context, teamID string, msg core.OutboundMessage) error {
	conn, err := r.in.conn()
	if err != nil {
		return err
	}
	ctx, cancel := r.h.callCtx(ctx)
	defer cancel()
	_, err = pb.NewResponderClient(conn).Reply(ctx, &pb.ReplyRequest{Provider: r.provider, TeamId: teamID, Message: wire.OutboundToPB(msg)})
	return wire.FromStatus(r.in.name(), err)
}

// SetIssueState implements core.IssueTracker. A plugin that is not a tracker
// returns UNIMPLEMENTED, which maps to core.ErrOperationNotSupported.
func (r *pluginResponder) SetIssueState(ctx context.Context, teamID string, issue core.SourceRef, state string) error {
	conn, err := r.in.conn()
	if err != nil {
		return err
	}
	ctx, cancel := r.h.callCtx(ctx)
	defer cancel()
	_, err = pb.NewResponderClient(conn).SetIssueState(ctx, &pb.SetIssueStateRequest{Provider: r.provider, TeamId: teamID, Issue: wire.SourceToPB(issue), State: state})
	return wire.FromStatus(r.in.name(), err)
}

// AttachPullRequest implements core.IssueTracker.
func (r *pluginResponder) AttachPullRequest(ctx context.Context, teamID string, issue core.SourceRef, pr core.PullRequestRef) error {
	conn, err := r.in.conn()
	if err != nil {
		return err
	}
	ctx, cancel := r.h.callCtx(ctx)
	defer cancel()
	_, err = pb.NewResponderClient(conn).AttachPullRequest(ctx, &pb.AttachPullRequestRequest{Provider: r.provider, TeamId: teamID, Issue: wire.SourceToPB(issue), PullRequest: wire.PRToPB(&pr)})
	return wire.FromStatus(r.in.name(), err)
}
