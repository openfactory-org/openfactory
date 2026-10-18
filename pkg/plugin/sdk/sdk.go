// Package sdk is the Go SDK for Open Factory plugins.
//
// EXPERIMENTAL: this package is not covered by the v1 compatibility promise.
// It may change in any release until it is declared stable in
// a v1.x release. The wire protocol (proto/openfactory/plugin/v1) is the
// stable contract; run `make go-apidiff` to see Go API changes.
//
// A plugin is a separate executable that the server launches (from
// OF_PLUGIN_DIR, files named openfactory-plugin-<name>) and talks to over
// gRPC (proto/openfactory/plugin/v1) using hashicorp/go-plugin. Plugins in
// other modules import only this package; the domain types they need (Forge,
// Responder, Event, SandboxBackend, ...) are re-exported here as aliases. A
// minimal plugin:
//
//	func main() {
//		host := &sdk.DeferredHost{}
//		sdk.Serve(sdk.Plugin{
//			Name: "example", Version: "0.1.0", Host: host,
//			ForgeProviders: []string{"example"},
//			Forge: func(ctx context.Context, t sdk.ForgeTarget) (sdk.Forge, error) { ... },
//			Ingress: myWebhookHandler, // an http.Handler; publish events with sdk.Emit
//		})
//	}
//
// The plugin reads integration records and their secrets through Host (see
// DeferredHost, Store, Secrets); the server links actors and publishes the
// events the Ingress handler emits. See examples/plugin-hello for a complete
// plugin built in its own module.
package sdk

import (
	"context"
	"net/http"
	"os"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/openfactory-org/openfactory/internal/core"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	pbv2 "github.com/openfactory-org/openfactory/pkg/plugin/pb/v2"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// ForgeTarget is the integration a Forge call acts for: the integration's
// non-secret config and the resolved credential.
type ForgeTarget struct {
	Provider      string
	IntegrationID string
	TeamID        string
	Config        map[string]string
	Token         string
	// UserToken is true when Token is a team member's personal token
	// (credentialStrategy CREATOR) rather than the integration's.
	UserToken bool
}

// ForgeFactory builds a core.Forge for one call. It should be cheap; the host
// sends the target with every request so plugins stay stateless.
type ForgeFactory func(ctx context.Context, t ForgeTarget) (core.Forge, error)

// IssueCommenter is implemented by forges whose issue comments differ from
// pull request comments (served as Forge.CommentIssue).
type IssueCommenter interface {
	CommentIssue(ctx context.Context, owner, repo string, number int, body string) error
}

// WorkItemCommenter is implemented by forges with tracker work items (served
// as Forge.CommentWorkItem).
type WorkItemCommenter interface {
	CommentWorkItem(ctx context.Context, project string, id int, body string) error
}

// WorkItemTracker is the tracker write-back of a forge with work items
// (served as Forge.SetWorkItemState and Forge.AttachPullRequestToWorkItem).
// state is core.IssueStateInProgress, core.IssueStateInReview or
// core.IssueStateDone.
type WorkItemTracker interface {
	SetWorkItemState(ctx context.Context, project string, id int, state string) error
	AttachPullRequestToWorkItem(ctx context.Context, project string, id int, pr core.PullRequestRef) error
}

// ReviewRequester is implemented by forges that can request a review of a
// pull request (served as Forge.RequestReviewers). Each login is a value that
// the same forge returns from LoginReader.
type ReviewRequester = core.ReviewRequester

// LoginReader is implemented by forges that can say which account their
// credential belongs to (served as Forge.Login). Together with
// ReviewRequester it lets the host request a review from a team member.
type LoginReader = core.LoginReader

// ReviewContextReader is implemented by forges that can read a review thread
// and the diff of a file of a pull request (served as Forge.GetReviewThread
// and Forge.GetFileDiff). The context of a review comment run uses it.
type ReviewContextReader = core.ReviewContextReader

// Plugin describes what a plugin serves. Unset components are not advertised.
type Plugin struct {
	// Name is the unique plugin name ([a-z0-9_-]+). Webhooks reach the plugin at
	// POST /plugins/{Name}/webhook.
	Name    string
	Version string
	// ConfigSchema is an optional JSON Schema for the Configure config map.
	ConfigSchema string

	// Forge serves code forges for ForgeProviders (integration provider names).
	Forge          ForgeFactory
	ForgeProviders []string

	// Responders post outbound messages for their Provider().
	Responders []core.Responder

	// Ingress handles inbound webhooks. Events published on Bus() during the
	// request are returned to the host, which publishes them.
	Ingress http.Handler

	// SandboxBackend is a sandbox backend
	// (openfactory.plugin.v2.SandboxBackend). It starts one sandbox for one
	// session of a run. The host names it "plugin:<Name>". Optional.
	SandboxBackend core.TaskBackend

	// IntegrationProviders are the integration providers whose records and
	// referenced secrets the plugin reads through Host. ForgeProviders are
	// always included.
	IntegrationProviders []string

	// Host is connected when the server configures the plugin. Optional.
	Host *DeferredHost
	// Configure is called after Host is connected, with the plugin's config
	// map, on every (re)start. Optional.
	Configure func(ctx context.Context, host Host, config map[string]string) error
}

// Serve runs the plugin. It blocks until the host kills the process. Starting
// the binary by hand (without the host's handshake environment) prints a
// message and exits.
func Serve(p Plugin) {
	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: wire.Handshake,
		Plugins:         wire.PluginSet(GRPCPlugin(p)),
		GRPCServer: func(opts []grpc.ServerOption) *grpc.Server {
			opts = append(opts, grpc.MaxRecvMsgSize(wire.MaxMessageBytes), grpc.MaxSendMsgSize(wire.MaxMessageBytes))
			return grpc.NewServer(opts...)
		},
		Logger: hclog.New(&hclog.LoggerOptions{Name: p.Name, Output: os.Stderr, JSONFormat: true, Level: hclog.Info}),
	})
}

// GRPCPlugin returns the go-plugin plugin serving p. Serve uses it; tests can
// use it with plugin.TestPluginGRPCConn to run a plugin in process.
func GRPCPlugin(p Plugin) *wire.GRPCPlugin {
	if p.Host == nil {
		p.Host = &DeferredHost{}
	}
	return &wire.GRPCPlugin{Register: func(broker *plugin.GRPCBroker, s *grpc.Server) error {
		pb.RegisterPluginServer(s, &pluginServer{p: p, broker: broker})
		if p.Forge != nil {
			pb.RegisterForgeServer(s, &forgeServer{factory: p.Forge})
		}
		if len(p.Responders) > 0 {
			pb.RegisterResponderServer(s, &responderServer{responders: p.Responders})
		}
		if p.Ingress != nil {
			pb.RegisterIngressServer(s, &ingressServer{h: p.Ingress})
		}
		if p.SandboxBackend != nil {
			pbv2.RegisterSandboxBackendServer(s, &sandboxServer{b: p.SandboxBackend})
		}
		return nil
	}}
}

type pluginServer struct {
	pb.UnimplementedPluginServer
	p      Plugin
	broker *plugin.GRPCBroker
}

func (s *pluginServer) GetInfo(context.Context, *pb.GetInfoRequest) (*pb.PluginInfo, error) {
	p := s.p
	info := &pb.PluginInfo{
		Name: p.Name, Version: p.Version, ProtocolVersion: wire.ProtocolVersion, ConfigSchema: p.ConfigSchema,
		ForgeProviders: p.ForgeProviders,
	}
	provs := map[string]bool{}
	addProv := func(v string) {
		if v != "" && !provs[v] {
			provs[v] = true
			info.IntegrationProviders = append(info.IntegrationProviders, v)
		}
	}
	for _, v := range p.IntegrationProviders {
		addProv(v)
	}
	if p.Forge != nil {
		info.Capabilities = append(info.Capabilities, wire.CapForge)
		for _, v := range p.ForgeProviders {
			addProv(v)
		}
	}
	if len(p.Responders) > 0 {
		info.Capabilities = append(info.Capabilities, wire.CapResponder)
		for _, r := range p.Responders {
			info.ResponderProviders = append(info.ResponderProviders, r.Provider())
		}
	}
	if p.Ingress != nil {
		info.Capabilities = append(info.Capabilities, wire.CapIngress)
	}
	if p.SandboxBackend != nil {
		info.Capabilities = append(info.Capabilities, wire.CapSandboxBackend)
	}
	return info, nil
}

func (s *pluginServer) Configure(ctx context.Context, req *pb.ConfigureRequest) (*pb.Empty, error) {
	conn, err := s.broker.Dial(req.HostBrokerId)
	if err != nil {
		return nil, wire.ToStatus(err)
	}
	s.p.Host.set(&grpcHost{c: pb.NewHostClient(conn), conn: conn})
	if s.p.Configure != nil {
		cfg := req.Config
		if cfg == nil {
			cfg = map[string]string{}
		}
		if err := s.p.Configure(ctx, s.p.Host, cfg); err != nil {
			return nil, wire.ToStatus(err)
		}
	}
	return &pb.Empty{}, nil
}

func (s *pluginServer) Ping(context.Context, *pb.Empty) (*pb.Empty, error) { return &pb.Empty{}, nil }
