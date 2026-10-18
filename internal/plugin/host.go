// Package plugin is the Open Factory plugin host. It discovers plugin
// executables, launches them with hashicorp/go-plugin (gRPC transport,
// automatic mTLS, magic cookie OF_PLUGIN), health-checks and restarts them, and
// exposes what they serve as core interfaces:
//
//   - forge.ForgeProvider (HasForge / ForgeFor) for forge.Resolver;
//   - core.Responder values (Responders);
//   - core.TaskBackend values (TaskBackend);
//   - an HTTP handler routing POST /plugins/{name}/webhook (and each plugin's
//     declared webhook aliases) to the plugin's Ingress service, publishing
//     the events it returns.
//
// The protocol is defined in proto/openfactory/plugin/v1; plugins are
// written with pkg/plugin/sdk.
package plugin

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"

	"github.com/openfactory-org/openfactory/internal/core"
	pb "github.com/openfactory-org/openfactory/pkg/plugin/pb"
	"github.com/openfactory-org/openfactory/pkg/plugin/wire"
)

// BinaryPrefix is the file name prefix of plugin executables discovered in Options.Dir.
const BinaryPrefix = "openfactory-plugin-"

// ErrUnavailable is returned while a plugin is not running (crashed and
// waiting to restart, or the host is closed).
var ErrUnavailable = errors.New("plugin unavailable")

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Options configures a Host.
type Options struct {
	// Dir is scanned for executables named openfactory-plugin-<name> (OF_PLUGIN_DIR).
	// A missing directory is not an error.
	Dir string
	// Paths are extra plugin executables (OF_PLUGINS), any file name.
	Paths []string
	// Config is passed to each plugin's Configure, by plugin name.
	Config map[string]map[string]string
	// EnvAllow lists the names of server environment variables that every
	// plugin process gets in addition to PATH, HOME, TMPDIR, LANG and TZ
	// (OF_PLUGIN_ENV_ALLOW). Server keys and credentials are never passed,
	// also when they are listed. See pluginEnv.
	EnvAllow []string
	// EnvByPlugin lists, for single plugins, the names of server environment
	// variables that only that plugin gets (OF_PLUGIN_ENV). The key is the
	// plugin name. The same rule about keys and credentials holds.
	EnvByPlugin map[string][]string
	// LookupEnv reads the server environment. Default os.LookupEnv.
	LookupEnv func(string) (string, bool)

	// Store backs the Host service (integrations, factories, actor linking).
	Store core.Store
	// Secrets backs Host.GetSecret / ExpandSecrets.
	Secrets core.SecretStore
	// FactoryRepos returns a factory's repositories (Host.GetFactoryRepos).
	// Optional; engine.LoadFactoryRuntime(...).Repositories in the server.
	FactoryRepos func(ctx context.Context, factoryID string) ([]core.RepoRef, error)
	// Publish receives each event returned by a plugin's Ingress. Required for
	// webhooks to have any effect.
	Publish func(ctx context.Context, ev core.Event) error

	Logger *slog.Logger

	// HealthInterval is the Ping period (default 15s). StartTimeout bounds
	// launch + handshake + GetInfo + Configure (default 30s). MinBackoff and
	// MaxBackoff bound restart delays (default 1s and 1m). CallTimeout bounds
	// each Forge/Responder/Ingress call without a deadline (default 2m).
	HealthInterval time.Duration
	StartTimeout   time.Duration
	MinBackoff     time.Duration
	MaxBackoff     time.Duration
	CallTimeout    time.Duration
}

// Info describes a running plugin.
type Info struct {
	Name                 string   `json:"name"`
	Version              string   `json:"version"`
	Path                 string   `json:"path"`
	Capabilities         []string `json:"capabilities"`
	ForgeProviders       []string `json:"forge_providers,omitempty"`
	ResponderProviders   []string `json:"responder_providers,omitempty"`
	IntegrationProviders []string `json:"integration_providers,omitempty"`
	ConfigSchema         string   `json:"config_schema,omitempty"`
	Running              bool     `json:"running"`
	Restarts             int      `json:"restarts"`
	LastError            string   `json:"last_error,omitempty"`
}

// Host manages plugin processes.
type Host struct {
	opts Options
	log  *slog.Logger

	mu        sync.RWMutex
	instances []*instance
	byName    map[string]*instance
	started   bool
	closed    bool

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a host. Call Start to launch plugins.
func New(opts Options) *Host {
	if opts.HealthInterval <= 0 {
		opts.HealthInterval = 15 * time.Second
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 30 * time.Second
	}
	if opts.MinBackoff <= 0 {
		opts.MinBackoff = time.Second
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = time.Minute
	}
	if opts.MaxBackoff < opts.MinBackoff {
		opts.MaxBackoff = opts.MinBackoff
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = 2 * time.Minute
	}
	if opts.LookupEnv == nil {
		opts.LookupEnv = os.LookupEnv
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Host{opts: opts, log: log.With("component", "plugin-host"), byName: map[string]*instance{}, ctx: ctx, cancel: cancel}
}

// Discover lists plugin executables: Options.Paths, then openfactory-plugin-*
// executables in Options.Dir (sorted, duplicates removed).
func (h *Host) Discover() ([]string, error) {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range h.opts.Paths {
		if p = strings.TrimSpace(p); p != "" {
			add(p)
		}
	}
	if h.opts.Dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(h.opts.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return out, fmt.Errorf("plugin: read %s: %w", h.opts.Dir, err)
	}
	var found []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, BinaryPrefix) || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(h.opts.Dir, name)
		st, err := os.Stat(path) // follows symlinks
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
			continue
		}
		found = append(found, path)
	}
	sort.Strings(found)
	for _, p := range found {
		add(p)
	}
	return out, nil
}

// Start discovers and launches every plugin and waits for each to be
// configured (or to fail its first start). Plugins that fail keep retrying in
// the background with backoff; their errors are joined in the returned error,
// which callers should log rather than treat as fatal.
func (h *Host) Start(ctx context.Context) error {
	paths, err := h.Discover()
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, p := range paths {
		if err := h.launch(ctx, p, execLauncher(h, p)); err != nil {
			errs = append(errs, err)
		}
	}
	h.mu.Lock()
	h.started = true
	h.mu.Unlock()
	return errors.Join(errs...)
}

// launch registers an instance, starts it once synchronously and then
// supervises it.
func (h *Host) launch(ctx context.Context, path string, l launcher) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrUnavailable
	}
	inst := &instance{host: h, path: path, launch: l}
	h.instances = append(h.instances, inst)
	h.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, h.opts.StartTimeout)
	err := inst.start(sctx)
	cancel()
	if err != nil {
		h.log.Error("plugin failed to start", "path", path, "err", err)
		if errors.Is(err, errFatal) {
			inst.setErr(err)
			return fmt.Errorf("plugin %s: %w", path, err)
		}
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		inst.supervise(h.ctx, err == nil)
	}()
	if err != nil {
		return fmt.Errorf("plugin %s: %w", path, err)
	}
	return nil
}

// register binds a name to an instance on its first successful start.
func (h *Host) register(inst *instance, name string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if other, ok := h.byName[name]; ok && other != inst {
		return fmt.Errorf("%w: plugin name %q already used by %s", errFatal, name, other.path)
	}
	h.byName[name] = inst
	return nil
}

// Plugins describes every known plugin.
func (h *Host) Plugins() []Info {
	h.mu.RLock()
	insts := append([]*instance(nil), h.instances...)
	h.mu.RUnlock()
	out := make([]Info, 0, len(insts))
	for _, in := range insts {
		out = append(out, in.describe())
	}
	return out
}

// plugin returns the named instance.
func (h *Host) plugin(name string) (*instance, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	in, ok := h.byName[name]
	return in, ok
}

func (h *Host) named() []*instance {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]*instance, 0, len(h.byName))
	for _, in := range h.byName {
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out
}

// Close stops supervision and kills every plugin process. Calls in flight fail.
func (h *Host) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	insts := append([]*instance(nil), h.instances...)
	h.mu.Unlock()
	h.cancel()
	h.wg.Wait()
	for _, in := range insts {
		in.stop()
	}
	return nil
}

// Shutdown is Close bounded by ctx.
func (h *Host) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() { _ = h.Close(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------- instances ----------

// errFatal marks start errors that restarting cannot fix.
var errFatal = errors.New("fatal plugin error")

// running is a started plugin process.
type running struct {
	conn   *wire.Conn
	kill   func()
	exited func() bool
	info   *pb.PluginInfo
	hostID uint32
}

// launcher starts a plugin process and returns its connection.
type launcher func(ctx context.Context) (conn *wire.Conn, kill func(), exited func() bool, err error)

func execLauncher(h *Host, path string) launcher {
	return func(ctx context.Context) (*wire.Conn, func(), func() bool, error) {
		logger := hclog.New(&hclog.LoggerOptions{Name: "plugin." + filepath.Base(path), Output: hclogWriter{h.log, filepath.Base(path)}, Level: hclog.Info, JSONFormat: true})
		c := goplugin.NewClient(&goplugin.ClientConfig{
			HandshakeConfig:  wire.Handshake,
			Plugins:          wire.PluginSet(&wire.GRPCPlugin{}),
			Cmd:              pluginCmd(h, path),
			SkipHostEnv:      true,
			AllowedProtocols: []goplugin.Protocol{goplugin.ProtocolGRPC},
			AutoMTLS:         true,
			Logger:           logger,
			StartTimeout:     h.opts.StartTimeout,
			GRPCDialOptions: []grpc.DialOption{grpc.WithDefaultCallOptions(
				grpc.MaxCallRecvMsgSize(wire.MaxMessageBytes), grpc.MaxCallSendMsgSize(wire.MaxMessageBytes))},
		})
		rpc, err := c.Client()
		if err != nil {
			c.Kill()
			return nil, nil, nil, err
		}
		raw, err := rpc.Dispense(wire.PluginKey)
		if err != nil {
			c.Kill()
			return nil, nil, nil, err
		}
		conn, ok := raw.(*wire.Conn)
		if !ok {
			c.Kill()
			return nil, nil, nil, fmt.Errorf("%w: unexpected plugin client %T", errFatal, raw)
		}
		return conn, c.Kill, c.Exited, nil
	}
}

// pluginCmd returns the command of a plugin process. Its environment holds
// only what pluginEnv gives, not the environment of the server.
func pluginCmd(h *Host, path string) *exec.Cmd {
	cmd := exec.Command(path) //nolint:noctx // the plugin client owns the process lifetime (Kill); no request context applies
	cmd.Env = pluginEnv(path, h.opts.EnvAllow, h.opts.EnvByPlugin, h.opts.LookupEnv)
	return cmd
}

type instance struct {
	host   *Host
	path   string
	launch launcher

	mu       sync.RWMutex
	cur      *running
	info     *pb.PluginInfo // last known info (kept while restarting)
	restarts int
	lastErr  string
}

func (in *instance) name() string {
	in.mu.RLock()
	defer in.mu.RUnlock()
	if in.info != nil {
		return in.info.Name
	}
	return filepath.Base(in.path)
}

func (in *instance) setErr(err error) {
	in.mu.Lock()
	in.lastErr = err.Error()
	in.mu.Unlock()
}

func (in *instance) describe() Info {
	in.mu.RLock()
	defer in.mu.RUnlock()
	inf := Info{Path: in.path, Running: in.cur != nil, Restarts: in.restarts, LastError: in.lastErr, Name: filepath.Base(in.path)}
	if i := in.info; i != nil {
		inf.Name, inf.Version, inf.Capabilities = i.Name, i.Version, i.Capabilities
		inf.ForgeProviders, inf.ResponderProviders, inf.IntegrationProviders = i.ForgeProviders, i.ResponderProviders, i.IntegrationProviders
		inf.ConfigSchema = i.ConfigSchema
	}
	return inf
}

// pluginInfo returns the last known info (nil before the first start).
func (in *instance) pluginInfo() *pb.PluginInfo {
	in.mu.RLock()
	defer in.mu.RUnlock()
	return in.info
}

// conn returns the live connection or ErrUnavailable.
func (in *instance) conn() (*grpc.ClientConn, error) {
	in.mu.RLock()
	defer in.mu.RUnlock()
	if in.cur == nil {
		return nil, fmt.Errorf("plugin %s: %w", in.nameLocked(), ErrUnavailable)
	}
	return in.cur.conn.Conn, nil
}

func (in *instance) nameLocked() string {
	if in.info != nil {
		return in.info.Name
	}
	return filepath.Base(in.path)
}

func (in *instance) start(ctx context.Context) error {
	conn, kill, exited, err := in.launch(ctx)
	if err != nil {
		return err
	}
	r := &running{conn: conn, kill: kill, exited: exited}
	fail := func(err error) error { kill(); return err }

	info, err := pb.NewPluginClient(conn.Conn).GetInfo(ctx, &pb.GetInfoRequest{HostProtocolVersion: wire.ProtocolVersion})
	if err != nil {
		return fail(fmt.Errorf("get info: %w", err))
	}
	if !validName.MatchString(info.Name) {
		return fail(fmt.Errorf("%w: invalid plugin name %q", errFatal, info.Name))
	}
	if info.ProtocolVersion != 0 && info.ProtocolVersion != wire.ProtocolVersion {
		return fail(fmt.Errorf("%w: plugin %s speaks protocol %d, host speaks %d", errFatal, info.Name, info.ProtocolVersion, wire.ProtocolVersion))
	}
	if prev := in.pluginInfo(); prev != nil && prev.Name != info.Name {
		return fail(fmt.Errorf("%w: plugin at %s changed its name from %q to %q", errFatal, in.path, prev.Name, info.Name))
	}
	if err := in.host.register(in, info.Name); err != nil {
		return fail(err)
	}
	r.info = info

	// Serve the Host service on the broker and hand its id to the plugin.
	svc := newHostService(in.host, info)
	r.hostID = conn.Broker.NextId()
	go conn.Broker.AcceptAndServe(r.hostID, func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(append(opts, grpc.MaxRecvMsgSize(wire.MaxMessageBytes), grpc.MaxSendMsgSize(wire.MaxMessageBytes))...)
		pb.RegisterHostServer(s, svc)
		return s
	})
	cfg := in.host.opts.Config[info.Name]
	if _, err := pb.NewPluginClient(conn.Conn).Configure(ctx, &pb.ConfigureRequest{HostBrokerId: r.hostID, Config: cfg}); err != nil {
		return fail(fmt.Errorf("configure: %w", err))
	}

	in.mu.Lock()
	in.cur, in.info, in.lastErr = r, info, ""
	in.mu.Unlock()
	in.host.log.Info("plugin started", "plugin", info.Name, "version", info.Version, "path", in.path, "capabilities", info.Capabilities)
	return nil
}

// stop kills the running process, if any.
func (in *instance) stop() {
	in.mu.Lock()
	r := in.cur
	in.cur = nil
	in.mu.Unlock()
	if r != nil {
		r.kill()
	}
}

// healthy checks process liveness and pings the plugin.
func (in *instance) healthy(ctx context.Context, ping bool) error {
	in.mu.RLock()
	r := in.cur
	in.mu.RUnlock()
	if r == nil {
		return ErrUnavailable
	}
	if r.exited() {
		return errors.New("plugin process exited")
	}
	if !ping {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := pb.NewPluginClient(r.conn.Conn).Ping(pctx, &pb.Empty{})
	return err
}

// supervise restarts the plugin when it dies or stops answering pings.
func (in *instance) supervise(ctx context.Context, up bool) {
	opts := in.host.opts
	backoff := opts.MinBackoff
	healthySince := time.Now()
	exitPoll := time.Second
	if opts.HealthInterval < exitPoll {
		exitPoll = opts.HealthInterval
	}
	lastPing := time.Now()
	for {
		if up {
			select {
			case <-ctx.Done():
				return
			case <-time.After(exitPoll):
			}
			ping := time.Since(lastPing) >= opts.HealthInterval
			if ping {
				lastPing = time.Now()
			}
			if err := in.healthy(ctx, ping); err != nil {
				if ctx.Err() != nil {
					return
				}
				in.host.log.Warn("plugin unhealthy; restarting", "plugin", in.name(), "err", err)
				in.setErr(err)
				in.stop()
				up = false
				if time.Since(healthySince) > opts.MaxBackoff {
					backoff = opts.MinBackoff
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, opts.MaxBackoff)
		sctx, cancel := context.WithTimeout(ctx, opts.StartTimeout)
		err := in.start(sctx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			in.setErr(err)
			in.host.log.Error("plugin restart failed", "plugin", in.name(), "err", err, "next_attempt_in", backoff)
			if errors.Is(err, errFatal) {
				return
			}
			continue
		}
		in.mu.Lock()
		in.restarts++
		in.mu.Unlock()
		up, healthySince, lastPing = true, time.Now(), time.Now()
	}
}

// callCtx applies the default call timeout when ctx has no deadline.
func (h *Host) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, h.opts.CallTimeout)
}

// hclogWriter forwards go-plugin's log lines (including plugin stderr) to slog.
type hclogWriter struct {
	log    *slog.Logger
	plugin string
}

func (w hclogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.log.Info(line, "plugin", w.plugin)
		}
	}
	return len(p), nil
}
