package plugin

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/openfactory-org/openfactory/internal/core"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// hostService implements pb.HostServer for one plugin. Access to
// integrations and secrets is limited to the plugin's integration providers.
type hostService struct {
	pb.UnimplementedHostServer
	h         *Host
	plugin    string
	providers map[string]bool
	log       *slog.Logger
}

func newHostService(h *Host, info *pb.PluginInfo) *hostService {
	provs := map[string]bool{}
	for _, p := range info.IntegrationProviders {
		provs[strings.ToLower(p)] = true
	}
	for _, p := range info.ForgeProviders {
		provs[strings.ToLower(p)] = true
	}
	return &hostService{h: h, plugin: info.Name, providers: provs, log: h.log.With("plugin", info.Name)}
}

func (s *hostService) store() (core.Store, error) {
	if s.h.opts.Store == nil {
		return nil, status.Error(codes.Unavailable, "host has no store")
	}
	return s.h.opts.Store, nil
}

func (s *hostService) Log(ctx context.Context, r *pb.LogRequest) (*pb.Empty, error) {
	level := slog.LevelInfo
	switch strings.ToLower(r.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	args := make([]any, 0, 2*len(r.Attrs))
	for k, v := range r.Attrs {
		args = append(args, k, v)
	}
	s.log.Log(ctx, level, r.Message, args...)
	return &pb.Empty{}, nil
}

func (s *hostService) ListTeams(ctx context.Context, _ *pb.Empty) (*pb.ListTeamsResponse, error) {
	st, err := s.store()
	if err != nil {
		return nil, err
	}
	teams, err := st.ListTeams(ctx)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	out := &pb.ListTeamsResponse{}
	for _, t := range teams {
		out.Teams = append(out.Teams, &pb.Team{Id: t.ID, Name: t.Name})
	}
	return out, nil
}

// integrations returns the permitted integration records of teamID (all teams when empty).
func (s *hostService) integrations(ctx context.Context, teamID, provider string) ([]core.Integration, error) {
	provider = strings.ToLower(provider)
	if provider != "" && !s.providers[provider] {
		return nil, status.Errorf(codes.PermissionDenied, "plugin %s may not read %q integrations", s.plugin, provider)
	}
	st, err := s.store()
	if err != nil {
		return nil, err
	}
	teams := []string{teamID}
	if teamID == "" {
		ts, err := st.ListTeams(ctx)
		if err != nil {
			return nil, wire.ToStatus(err)
		}
		teams = teams[:0]
		for _, t := range ts {
			teams = append(teams, t.ID)
		}
	}
	var out []core.Integration
	for _, tid := range teams {
		ints, err := st.ListIntegrations(ctx, tid)
		if err != nil {
			return nil, wire.ToStatus(err)
		}
		for _, in := range ints {
			p := strings.ToLower(in.Provider)
			if s.providers[p] && (provider == "" || p == provider) {
				out = append(out, in)
			}
		}
	}
	return out, nil
}

func (s *hostService) ListIntegrations(ctx context.Context, r *pb.ListIntegrationsRequest) (*pb.ListIntegrationsResponse, error) {
	ints, err := s.integrations(ctx, r.TeamId, r.Provider)
	if err != nil {
		return nil, err
	}
	out := &pb.ListIntegrationsResponse{}
	for _, in := range ints {
		out.Integrations = append(out.Integrations, wire.IntegrationToPB(in))
	}
	return out, nil
}

// secretAllowed reports whether one of the plugin's integration records in
// the team references the secret (SecretRef or a config value under a key
// ending in "_ref").
func (s *hostService) secretAllowed(ctx context.Context, teamID, name string) (bool, error) {
	if teamID == "" || name == "" {
		return false, nil
	}
	ints, err := s.integrations(ctx, teamID, "")
	if err != nil {
		return false, err
	}
	for _, in := range ints {
		if in.SecretRef == name {
			return true, nil
		}
		for k, v := range in.Config {
			if strings.HasSuffix(k, "_ref") && v == name {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *hostService) secret(ctx context.Context, teamID, factoryID, name string) ([]byte, error) {
	ok, err := s.secretAllowed(ctx, teamID, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, status.Errorf(codes.PermissionDenied, "plugin %s may not read secret %q of team %s: no %s integration references it",
			s.plugin, name, teamID, s.plugin)
	}
	if s.h.opts.Secrets == nil {
		return nil, status.Error(codes.Unavailable, "host has no secret store")
	}
	v, err := s.h.opts.Secrets.Get(ctx, teamID, factoryID, name)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return v, nil
}

func (s *hostService) GetSecret(ctx context.Context, r *pb.GetSecretRequest) (*pb.GetSecretResponse, error) {
	v, err := s.secret(ctx, r.TeamId, r.FactoryId, r.Name)
	if err != nil {
		return nil, err
	}
	return &pb.GetSecretResponse{Value: v}, nil
}

var placeholder = regexp.MustCompile(`\{\{\s*([A-Za-z_][A-Za-z0-9_./-]*)\s*\}\}`)

func (s *hostService) ExpandSecrets(ctx context.Context, r *pb.ExpandSecretsRequest) (*pb.ExpandSecretsResponse, error) {
	var firstErr error
	out := placeholder.ReplaceAllStringFunc(r.Text, func(m string) string {
		if firstErr != nil {
			return m
		}
		name := placeholder.FindStringSubmatch(m)[1]
		v, err := s.secret(ctx, r.TeamId, r.FactoryId, name)
		if err != nil {
			firstErr = err
			return m
		}
		return string(v)
	})
	if firstErr != nil {
		return nil, firstErr
	}
	return &pb.ExpandSecretsResponse{Text: out}, nil
}

func factoryToPB(f core.Factory) *pb.Factory {
	return &pb.Factory{Id: f.ID, TeamId: f.TeamID, Alias: f.Alias, Name: f.Name}
}

func (s *hostService) ListFactories(ctx context.Context, r *pb.ListFactoriesRequest) (*pb.ListFactoriesResponse, error) {
	if r.TeamId == "" {
		return nil, status.Error(codes.InvalidArgument, "team_id is required")
	}
	st, err := s.store()
	if err != nil {
		return nil, err
	}
	fs, err := st.ListFactories(ctx, r.TeamId, "")
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	out := &pb.ListFactoriesResponse{}
	for _, f := range fs {
		out.Factories = append(out.Factories, factoryToPB(f))
	}
	return out, nil
}

func (s *hostService) GetFactory(ctx context.Context, r *pb.GetFactoryRequest) (*pb.Factory, error) {
	st, err := s.store()
	if err != nil {
		return nil, err
	}
	f, err := st.GetFactory(ctx, r.FactoryId)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	return factoryToPB(*f), nil
}

func (s *hostService) GetFactoryRepos(ctx context.Context, r *pb.GetFactoryReposRequest) (*pb.GetFactoryReposResponse, error) {
	if s.h.opts.FactoryRepos == nil {
		return nil, wire.ToStatus(errors.Join(core.ErrNotFound, errors.New("host does not expose factory repositories")))
	}
	repos, err := s.h.opts.FactoryRepos(ctx, r.FactoryId)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	out := &pb.GetFactoryReposResponse{}
	for _, x := range repos {
		out.Repos = append(out.Repos, &pb.RepoRef{CodeForge: x.CodeForge, Owner: x.Owner, Name: x.Name})
	}
	return out, nil
}
