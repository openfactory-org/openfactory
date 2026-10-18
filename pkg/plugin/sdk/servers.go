package sdk

import (
	"bytes"
	"context"
	"net/http"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openfactory-org/openfactory/internal/core"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// ---------- Forge ----------

type forgeServer struct {
	pb.UnimplementedForgeServer
	factory ForgeFactory
}

func (s *forgeServer) forge(ctx context.Context, t *pb.ForgeTarget) (core.Forge, error) {
	if t == nil {
		return nil, status.Error(codes.InvalidArgument, "target is required")
	}
	cfg := t.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	f, err := s.factory(ctx, ForgeTarget{Provider: t.Provider, IntegrationID: t.IntegrationId, TeamID: t.TeamId, Config: cfg, Token: t.Token, UserToken: t.UserToken})
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return f, nil
}

func (s *forgeServer) Name(ctx context.Context, r *pb.ForgeNameRequest) (*pb.ForgeNameResponse, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	return &pb.ForgeNameResponse{Name: f.Name()}, nil
}

func (s *forgeServer) CloneURL(ctx context.Context, r *pb.RepoRequest) (*pb.CloneURLResponse, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	u, err := f.CloneURL(ctx, r.Owner, r.Repo)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return &pb.CloneURLResponse{Url: u}, nil
}

func (s *forgeServer) DefaultBranch(ctx context.Context, r *pb.RepoRequest) (*pb.DefaultBranchResponse, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	b, err := f.DefaultBranch(ctx, r.Owner, r.Repo)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return &pb.DefaultBranchResponse{Branch: b}, nil
}

func (s *forgeServer) CreatePullRequest(ctx context.Context, r *pb.CreatePullRequestRequest) (*pb.PullRequestRef, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	pr, err := f.CreatePullRequest(ctx, core.PullRequestInput{Owner: r.Owner, Repo: r.Repo, Head: r.Head, Base: r.Base, Title: r.Title, Body: r.Body, Draft: r.Draft})
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return wire.PRToPB(pr), nil
}

func (s *forgeServer) GetPullRequest(ctx context.Context, r *pb.GetPullRequestRequest) (*pb.PullRequestRef, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	pr, err := f.GetPullRequest(ctx, r.Owner, r.Repo, int(r.Number))
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return wire.PRToPB(pr), nil
}

func (s *forgeServer) Comment(ctx context.Context, r *pb.CommentRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, wire.ToStatus(f.Comment(ctx, r.Owner, r.Repo, int(r.Number), r.Body))
}

func (s *forgeServer) CommentIssue(ctx context.Context, r *pb.CommentRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	ic, ok := f.(IssueCommenter)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge does not distinguish issue comments")
	}
	return &pb.Empty{}, wire.ToStatus(ic.CommentIssue(ctx, r.Owner, r.Repo, int(r.Number), r.Body))
}

func (s *forgeServer) CommentWorkItem(ctx context.Context, r *pb.CommentWorkItemRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	wc, ok := f.(WorkItemCommenter)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge has no work items")
	}
	return &pb.Empty{}, wire.ToStatus(wc.CommentWorkItem(ctx, r.Project, int(r.Id), r.Body))
}

func (s *forgeServer) SetWorkItemState(ctx context.Context, r *pb.SetWorkItemStateRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	wt, ok := f.(WorkItemTracker)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge has no work items")
	}
	return &pb.Empty{}, wire.ToStatus(wt.SetWorkItemState(ctx, r.Project, int(r.Id), r.State))
}

func (s *forgeServer) AttachPullRequestToWorkItem(ctx context.Context, r *pb.AttachPullRequestToWorkItemRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	wt, ok := f.(WorkItemTracker)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge has no work items")
	}
	var pr core.PullRequestRef
	if p := wire.PRFromPB(r.PullRequest); p != nil {
		pr = *p
	}
	return &pb.Empty{}, wire.ToStatus(wt.AttachPullRequestToWorkItem(ctx, r.Project, int(r.Id), pr))
}

func (s *forgeServer) SetCommitStatus(ctx context.Context, r *pb.SetCommitStatusRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	var st core.CommitStatus
	if r.Status != nil {
		st = core.CommitStatus{Context: r.Status.Context, State: r.Status.State, Description: r.Status.Description, TargetURL: r.Status.TargetUrl}
	}
	return &pb.Empty{}, wire.ToStatus(f.SetCommitStatus(ctx, r.Owner, r.Repo, r.Sha, st))
}

func (s *forgeServer) GetFiles(ctx context.Context, r *pb.GetFilesRequest) (*pb.GetFilesResponse, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	files, sha, err := f.GetFiles(ctx, r.Owner, r.Repo, r.Ref, r.Path)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return &pb.GetFilesResponse{Files: files, CommitSha: sha}, nil
}

func (s *forgeServer) ReplyInThread(ctx context.Context, r *pb.ThreadRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, wire.ToStatus(f.ReplyInThread(ctx, r.Owner, r.Repo, int(r.Number), r.ThreadRef, r.Body))
}

func (s *forgeServer) ResolveThread(ctx context.Context, r *pb.ThreadRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, wire.ToStatus(f.ResolveThread(ctx, r.Owner, r.Repo, int(r.Number), r.ThreadRef))
}

func (s *forgeServer) SubmitReview(ctx context.Context, r *pb.SubmitReviewRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	in := core.ReviewInput{Owner: r.Owner, Repo: r.Repo, Number: int(r.Number), Event: r.Event, Body: r.Body}
	for _, c := range r.Comments {
		in.Comments = append(in.Comments, core.ReviewComment{Path: c.Path, Line: int(c.Line), Side: c.Side, Body: c.Body})
	}
	return &pb.Empty{}, wire.ToStatus(f.SubmitReview(ctx, in))
}

func (s *forgeServer) MarkReady(ctx context.Context, r *pb.GetPullRequestRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, wire.ToStatus(f.MarkReady(ctx, r.Owner, r.Repo, int(r.Number)))
}

func (s *forgeServer) RequestReviewers(ctx context.Context, r *pb.RequestReviewersRequest) (*pb.Empty, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	rr, ok := f.(ReviewRequester)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge cannot request a review")
	}
	return &pb.Empty{}, wire.ToStatus(rr.RequestReviewers(ctx, r.Owner, r.Repo, int(r.Number), r.Logins))
}

func (s *forgeServer) Login(ctx context.Context, r *pb.LoginRequest) (*pb.LoginResponse, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	lr, ok := f.(LoginReader)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge cannot read the login of a credential")
	}
	login, err := lr.Login(ctx)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return &pb.LoginResponse{Login: login}, nil
}

func (s *forgeServer) GetReviewThread(ctx context.Context, r *pb.ThreadRequest) (*pb.ReviewThread, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	rc, ok := f.(ReviewContextReader)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge cannot read a review thread")
	}
	th, err := rc.ReviewThread(ctx, r.Owner, r.Repo, int(r.Number), r.ThreadRef)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return wire.ReviewThreadToPB(th), nil
}

func (s *forgeServer) GetFileDiff(ctx context.Context, r *pb.FileDiffRequest) (*pb.FileDiff, error) {
	f, err := s.forge(ctx, r.Target)
	if err != nil {
		return nil, err
	}
	rc, ok := f.(ReviewContextReader)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "forge cannot read the diff of a file")
	}
	d, err := rc.FileDiff(ctx, r.Owner, r.Repo, int(r.Number), r.Path)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return &pb.FileDiff{Path: d.Path, Patch: d.Patch, Truncated: d.Truncated}, nil
}

// ---------- Responder ----------

type responderServer struct {
	pb.UnimplementedResponderServer
	responders []core.Responder
}

func (s *responderServer) Reply(ctx context.Context, r *pb.ReplyRequest) (*pb.Empty, error) {
	for _, rs := range s.responders {
		if rs.Provider() == r.Provider {
			return &pb.Empty{}, wire.ToStatus(rs.Reply(ctx, r.TeamId, wire.OutboundFromPB(r.Message)))
		}
	}
	return nil, status.Errorf(codes.NotFound, "no responder for provider %q", r.Provider)
}

func (s *responderServer) tracker(provider string) (core.IssueTracker, error) {
	for _, rs := range s.responders {
		if rs.Provider() == provider {
			t, ok := rs.(core.IssueTracker)
			if !ok {
				return nil, status.Errorf(codes.Unimplemented, "responder %q is not an issue tracker", provider)
			}
			return t, nil
		}
	}
	return nil, status.Errorf(codes.NotFound, "no responder for provider %q", provider)
}

func (s *responderServer) SetIssueState(ctx context.Context, r *pb.SetIssueStateRequest) (*pb.Empty, error) {
	t, err := s.tracker(r.Provider)
	if err != nil {
		return nil, err
	}
	return &pb.Empty{}, wire.ToStatus(t.SetIssueState(ctx, r.TeamId, wire.SourceFromPB(r.Issue), r.State))
}

func (s *responderServer) AttachPullRequest(ctx context.Context, r *pb.AttachPullRequestRequest) (*pb.Empty, error) {
	t, err := s.tracker(r.Provider)
	if err != nil {
		return nil, err
	}
	pr := wire.PRFromPB(r.PullRequest)
	if pr == nil {
		return nil, status.Error(codes.InvalidArgument, "pull_request is required")
	}
	return &pb.Empty{}, wire.ToStatus(t.AttachPullRequest(ctx, r.TeamId, wire.SourceFromPB(r.Issue), *pr))
}

// ---------- Ingress ----------

type emitKey struct{}

type collector struct {
	mu  sync.Mutex
	evs []*core.Event
}

// Bus returns a core.EventBus for Ingress handlers: events published on
// core.TopicInboundEvent during a webhook request are returned to the server,
// which links actors, resolves factory aliases and publishes them. Other
// topics, and publishes outside a request, are dropped. Subscribe returns a
// closed channel.
func Bus() core.EventBus { return emitBus{} }

type emitBus struct{}

func (emitBus) Publish(ctx context.Context, topic string, payload any) {
	c, _ := ctx.Value(emitKey{}).(*collector)
	ev, ok := payload.(*core.Event)
	if c == nil || !ok || topic != core.TopicInboundEvent {
		return
	}
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
}

func (emitBus) Subscribe(string) (<-chan any, func()) {
	ch := make(chan any)
	close(ch)
	return ch, func() {}
}

// Emit adds events to the current webhook request's result (same as
// publishing them on Bus()).
func Emit(ctx context.Context, evs ...*core.Event) {
	for _, ev := range evs {
		emitBus{}.Publish(ctx, core.TopicInboundEvent, ev)
	}
}

type ingressServer struct {
	pb.UnimplementedIngressServer
	h http.Handler
}

type recorder struct {
	hdr    http.Header
	status int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.hdr }

func (r *recorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}

func (r *recorder) Write(b []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	return r.body.Write(b)
}

func (s *ingressServer) HandleWebhook(ctx context.Context, r *pb.WebhookRequest) (*pb.WebhookResponse, error) {
	col := &collector{}
	ctx = context.WithValue(ctx, emitKey{}, col)
	target := "http://plugin" + r.Path
	if r.RawQuery != "" {
		target += "?" + r.RawQuery
	}
	method := r.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(r.Body))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	req.Header = wire.HeadersFromPB(r.Headers)
	req.Host = r.Host
	req.RemoteAddr = r.RemoteAddr
	req.ContentLength = int64(len(r.Body))
	rec := &recorder{hdr: http.Header{}}
	s.h.ServeHTTP(rec, req)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	resp := &pb.WebhookResponse{Status: wire.ClampInt32(rec.status), Headers: wire.HeadersToPB(rec.hdr), Body: rec.body.Bytes()}
	col.mu.Lock()
	defer col.mu.Unlock()
	for _, ev := range col.evs {
		pe, err := wire.EventToPB(ev)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode event: %v", err)
		}
		resp.Events = append(resp.Events, pe)
	}
	return resp, nil
}
