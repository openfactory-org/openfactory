package sdk

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"

	"google.golang.org/grpc"

	"github.com/openfactory-org/openfactory/internal/core"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// ErrNotConnected is returned by DeferredHost before the server configured the plugin.
var ErrNotConnected = errors.New("plugin: host not connected yet")

// Host is the server-side API available to a plugin. Access is scoped to the
// plugin's integration providers (see Plugin.IntegrationProviders).
type Host interface {
	Log(ctx context.Context, level, msg string, attrs map[string]string)
	// Secret reads a secret referenced by one of the plugin's integration records.
	Secret(ctx context.Context, teamID, factoryID, name string) ([]byte, error)
	// Expand replaces {{SECRET_NAME}} placeholders (same scoping as Secret).
	Expand(ctx context.Context, teamID, factoryID, s string) (string, error)
	Teams(ctx context.Context) ([]core.Team, error)
	// Integrations lists integration records; empty teamID means all teams and
	// empty provider means every provider the plugin may read.
	Integrations(ctx context.Context, teamID, provider string) ([]core.Integration, error)
	Factories(ctx context.Context, teamID string) ([]core.Factory, error)
	Factory(ctx context.Context, factoryID string) (*core.Factory, error)
	// FactoryRepos returns the repositories of a factory's active definition.
	FactoryRepos(ctx context.Context, factoryID string) ([]core.RepoRef, error)
}

// DeferredHost is a Host that becomes usable once the server configures the
// plugin. Components can hold it from construction time.
type DeferredHost struct {
	mu   sync.RWMutex
	h    Host
	conn *grpc.ClientConn
}

var _ Host = (*DeferredHost)(nil)

func (d *DeferredHost) set(h *grpcHost) {
	d.mu.Lock()
	old := d.conn
	d.h, d.conn = h, h.conn
	d.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// Set connects d to h (for tests and in-process use).
func (d *DeferredHost) Set(h Host) {
	d.mu.Lock()
	d.h = h
	d.mu.Unlock()
}

func (d *DeferredHost) get() (Host, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.h == nil {
		return nil, ErrNotConnected
	}
	return d.h, nil
}

// Connected reports whether the host is connected.
func (d *DeferredHost) Connected() bool { _, err := d.get(); return err == nil }

func (d *DeferredHost) Log(ctx context.Context, level, msg string, attrs map[string]string) {
	h, err := d.get()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s %v\n", level, msg, attrs)
		return
	}
	h.Log(ctx, level, msg, attrs)
}

func (d *DeferredHost) Secret(ctx context.Context, teamID, factoryID, name string) ([]byte, error) {
	h, err := d.get()
	if err != nil {
		return nil, err
	}
	return h.Secret(ctx, teamID, factoryID, name)
}

func (d *DeferredHost) Expand(ctx context.Context, teamID, factoryID, s string) (string, error) {
	h, err := d.get()
	if err != nil {
		return "", err
	}
	return h.Expand(ctx, teamID, factoryID, s)
}

func (d *DeferredHost) Teams(ctx context.Context) ([]core.Team, error) {
	h, err := d.get()
	if err != nil {
		return nil, err
	}
	return h.Teams(ctx)
}

func (d *DeferredHost) Integrations(ctx context.Context, teamID, provider string) ([]core.Integration, error) {
	h, err := d.get()
	if err != nil {
		return nil, err
	}
	return h.Integrations(ctx, teamID, provider)
}

func (d *DeferredHost) Factories(ctx context.Context, teamID string) ([]core.Factory, error) {
	h, err := d.get()
	if err != nil {
		return nil, err
	}
	return h.Factories(ctx, teamID)
}

func (d *DeferredHost) Factory(ctx context.Context, factoryID string) (*core.Factory, error) {
	h, err := d.get()
	if err != nil {
		return nil, err
	}
	return h.Factory(ctx, factoryID)
}

func (d *DeferredHost) FactoryRepos(ctx context.Context, factoryID string) ([]core.RepoRef, error) {
	h, err := d.get()
	if err != nil {
		return nil, err
	}
	return h.FactoryRepos(ctx, factoryID)
}

// grpcHost calls the server's Host service.
type grpcHost struct {
	c    pb.HostClient
	conn *grpc.ClientConn
}

func remote(err error) error { return wire.FromStatus("host", err) }

func (g *grpcHost) Log(ctx context.Context, level, msg string, attrs map[string]string) {
	_, _ = g.c.Log(ctx, &pb.LogRequest{Level: level, Message: msg, Attrs: attrs})
}

func (g *grpcHost) Secret(ctx context.Context, teamID, factoryID, name string) ([]byte, error) {
	r, err := g.c.GetSecret(ctx, &pb.GetSecretRequest{TeamId: teamID, FactoryId: factoryID, Name: name})
	if err != nil {
		return nil, remote(err)
	}
	return r.Value, nil
}

func (g *grpcHost) Expand(ctx context.Context, teamID, factoryID, s string) (string, error) {
	r, err := g.c.ExpandSecrets(ctx, &pb.ExpandSecretsRequest{TeamId: teamID, FactoryId: factoryID, Text: s})
	if err != nil {
		return "", remote(err)
	}
	return r.Text, nil
}

func (g *grpcHost) Teams(ctx context.Context) ([]core.Team, error) {
	r, err := g.c.ListTeams(ctx, &pb.Empty{})
	if err != nil {
		return nil, remote(err)
	}
	out := make([]core.Team, 0, len(r.Teams))
	for _, t := range r.Teams {
		out = append(out, core.Team{ID: t.Id, Name: t.Name})
	}
	return out, nil
}

func (g *grpcHost) Integrations(ctx context.Context, teamID, provider string) ([]core.Integration, error) {
	r, err := g.c.ListIntegrations(ctx, &pb.ListIntegrationsRequest{TeamId: teamID, Provider: provider})
	if err != nil {
		return nil, remote(err)
	}
	out := make([]core.Integration, 0, len(r.Integrations))
	for _, in := range r.Integrations {
		out = append(out, wire.IntegrationFromPB(in))
	}
	return out, nil
}

func factoryFromPB(f *pb.Factory) core.Factory {
	return core.Factory{ID: f.Id, TeamID: f.TeamId, Alias: f.Alias, Name: f.Name}
}

func (g *grpcHost) Factories(ctx context.Context, teamID string) ([]core.Factory, error) {
	r, err := g.c.ListFactories(ctx, &pb.ListFactoriesRequest{TeamId: teamID})
	if err != nil {
		return nil, remote(err)
	}
	out := make([]core.Factory, 0, len(r.Factories))
	for _, f := range r.Factories {
		out = append(out, factoryFromPB(f))
	}
	return out, nil
}

func (g *grpcHost) Factory(ctx context.Context, factoryID string) (*core.Factory, error) {
	r, err := g.c.GetFactory(ctx, &pb.GetFactoryRequest{FactoryId: factoryID})
	if err != nil {
		return nil, remote(err)
	}
	f := factoryFromPB(r)
	return &f, nil
}

func (g *grpcHost) FactoryRepos(ctx context.Context, factoryID string) ([]core.RepoRef, error) {
	r, err := g.c.GetFactoryRepos(ctx, &pb.GetFactoryReposRequest{FactoryId: factoryID})
	if err != nil {
		return nil, remote(err)
	}
	out := make([]core.RepoRef, 0, len(r.Repos))
	for _, x := range r.Repos {
		out = append(out, core.RepoRef{CodeForge: x.CodeForge, Owner: x.Owner, Name: x.Name})
	}
	return out, nil
}

// ---------- adapters for code written against core interfaces ----------

// Store returns a read-only core.Store subset backed by h, so integration code
// written for the server (forgeutil.FindIntegrations, GetFactoryByAlias...)
// runs unchanged in a plugin. Implemented: ListTeams, ListIntegrations,
// GetFactory, GetFactoryByAlias, ListFactories and FindExternalIdentity (always
// core.ErrNotFound: the server links actors after the plugin returns events).
// Any other method panics.
func Store(h Host) core.Store { return &hostStore{h: h} }

type hostStore struct {
	core.Store
	h Host
}

func (s *hostStore) ListTeams(ctx context.Context) ([]core.Team, error) { return s.h.Teams(ctx) }

func (s *hostStore) ListIntegrations(ctx context.Context, teamID string) ([]core.Integration, error) {
	return s.h.Integrations(ctx, teamID, "")
}

func (s *hostStore) GetFactory(ctx context.Context, id string) (*core.Factory, error) {
	return s.h.Factory(ctx, id)
}

func (s *hostStore) GetFactoryByAlias(ctx context.Context, teamID, alias string) (*core.Factory, error) {
	fs, err := s.h.Factories(ctx, teamID)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if strings.EqualFold(f.Alias, alias) {
			return &f, nil
		}
	}
	return nil, core.ErrNotFound
}

func (s *hostStore) ListFactories(ctx context.Context, teamID, search string) ([]core.Factory, error) {
	fs, err := s.h.Factories(ctx, teamID)
	if err != nil || search == "" {
		return fs, err
	}
	var out []core.Factory
	for _, f := range fs {
		if strings.Contains(strings.ToLower(f.Name+" "+f.Alias), strings.ToLower(search)) {
			out = append(out, f)
		}
	}
	return out, nil
}

func (s *hostStore) FindExternalIdentity(context.Context, string, string) (*core.ExternalIdentity, error) {
	return nil, core.ErrNotFound
}

func (s *hostStore) Close() error { return nil }

// Secrets returns a core.SecretStore backed by h. Only Get and Expand work;
// plugins cannot write secrets.
func Secrets(h Host) core.SecretStore { return hostSecrets{h: h} }

type hostSecrets struct{ h Host }

var errReadOnly = fmt.Errorf("%w: plugins cannot modify secrets", core.ErrForbidden)

func (s hostSecrets) Put(context.Context, string, string, string, core.SecretType, []byte) (*core.Secret, error) {
	return nil, errReadOnly
}

func (s hostSecrets) Get(ctx context.Context, teamID, factoryID, name string) ([]byte, error) {
	return s.h.Secret(ctx, teamID, factoryID, name)
}

func (s hostSecrets) List(context.Context, string, string) ([]core.Secret, error) {
	return nil, errReadOnly
}

func (s hostSecrets) Delete(context.Context, string) error { return errReadOnly }

func (s hostSecrets) Expand(ctx context.Context, teamID, factoryID, str string) (string, error) {
	return s.h.Expand(ctx, teamID, factoryID, str)
}

// Logger returns a slog.Logger whose records go to the server log through h.
func Logger(h Host) *slog.Logger { return slog.New(&hostHandler{h: h}) }

type hostHandler struct {
	h     Host
	attrs []slog.Attr
	group string
}

func (l *hostHandler) Enabled(context.Context, slog.Level) bool { return true }

func (l *hostHandler) Handle(ctx context.Context, r slog.Record) error {
	attrs := map[string]string{}
	add := func(a slog.Attr) {
		k := a.Key
		if l.group != "" {
			k = l.group + "." + k
		}
		attrs[k] = a.Value.String()
	}
	for _, a := range l.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	level := "info"
	switch {
	case r.Level >= slog.LevelError:
		level = "error"
	case r.Level >= slog.LevelWarn:
		level = "warn"
	case r.Level < slog.LevelInfo:
		level = "debug"
	}
	l.h.Log(context.WithoutCancel(ctx), level, r.Message, attrs)
	return nil
}

func (l *hostHandler) WithAttrs(as []slog.Attr) slog.Handler {
	n := *l
	n.attrs = append(append([]slog.Attr(nil), l.attrs...), as...)
	return &n
}

func (l *hostHandler) WithGroup(name string) slog.Handler {
	n := *l
	if n.group != "" {
		name = n.group + "." + name
	}
	n.group = name
	return &n
}
