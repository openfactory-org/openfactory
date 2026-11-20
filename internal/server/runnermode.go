package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openfactory-org/openfactory/internal/auth"
	"github.com/openfactory-org/openfactory/internal/cloudid"
	"github.com/openfactory-org/openfactory/internal/config"
	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/dispatch"
	"github.com/openfactory-org/openfactory/internal/engine"
	"github.com/openfactory-org/openfactory/internal/keys"
	"github.com/openfactory-org/openfactory/internal/objectstore"
	"github.com/openfactory-org/openfactory/internal/runnerapi"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/runnertoken"
	"github.com/openfactory-org/openfactory/internal/sandbox"
	"github.com/openfactory-org/openfactory/internal/sandbox/k8stask"
	"github.com/openfactory-org/openfactory/internal/sandbox/kubernetes"
	"github.com/openfactory-org/openfactory/internal/sandbox/tasklocal"
	"github.com/openfactory-org/openfactory/internal/store/coord"
)

const (
	runnerSetupTimeoutSec = 1800
	runnerMaxSessionSec   = 8 * 3600
	runnerEventBatchBytes = 1 << 20
)

// runnerMode holds the parts of the runner path: the dispatcher, the runner API,
// the task backend and the session content.
type runnerMode struct {
	dispatcher *dispatch.Dispatcher
	service    *runnerapi.Service
	backend    core.TaskBackend
	content    *runnerContent
	keys       runnertoken.KeySet
	store      core.RunnerStore
	router     *lateRouter
	// ownsTasks is true when the tasks are children of this process (the local
	// backend). A Job of the kubernetes backend, or a task of a plugin, outlives
	// a replica and belongs to every replica, so a replica must not stop it.
	ownsTasks bool
}

// lateRouter is the router of the dispatcher. The worker hub does not exist
// when the dispatcher is built, so the server sets it later.
type lateRouter struct {
	mu sync.RWMutex
	r  dispatch.BackendRouter
}

func (l *lateRouter) set(r dispatch.BackendRouter) {
	l.mu.Lock()
	l.r = r
	l.mu.Unlock()
}

func (l *lateRouter) get() dispatch.BackendRouter {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.r
}

// BackendFor implements dispatch.BackendRouter.
func (l *lateRouter) BackendFor(run *core.Run) (core.TaskBackend, bool) {
	if r := l.get(); r != nil {
		return r.BackendFor(run)
	}
	return nil, false
}

// Backends implements dispatch.BackendRouter.
func (l *lateRouter) Backends() []core.TaskBackend {
	if r := l.get(); r != nil {
		return r.Backends()
	}
	return nil
}

// setRouter gives the dispatcher the backends of remote workers.
func (m *runnerMode) setRouter(r dispatch.BackendRouter) {
	if m != nil {
		m.router.set(r)
	}
}

// BuildTaskBackend builds the task backend of cfg.SandboxBackend for a worker
// process and returns the default image of its tasks.
func BuildTaskBackend(cfg *config.Config, log *slog.Logger) (core.TaskBackend, string, error) {
	return buildTaskBackend(cfg, nil, log)
}

// runnerKeySet builds the key set of the session tokens. The current signing
// key signs. Each previous signing key only verifies. The key ID of a key is
// its version (keys.Version of the derived key).
func runnerKeySet(signKey []byte, signPrev [][]byte) (*runnertoken.StaticKeys, error) {
	cur, prev := config.DeriveKeys(signKey, signPrev, config.LabelRunnerToken)
	kid := keys.Version(cur)
	all := map[string][]byte{kid: cur}
	for _, k := range prev {
		all[keys.Version(k)] = k
	}
	return runnertoken.NewStaticKeys(kid, all)
}

// taskBackendSource finds the task backend of a plugin by plugin name. The
// plugin host implements it.
type taskBackendSource interface {
	TaskBackend(name string) (core.TaskBackend, error)
}

// newRunnerMode builds the dispatcher and the runner API. It must run before
// the engine is built, because the engine sends messages through the dispatcher.
// buildTaskBackend returns the task backend of OF_SANDBOX_BACKEND and the
// image of each task. The local backend starts the runner on the host. The
// kubernetes backend starts one Job for each task.
func buildTaskBackend(cfg *config.Config, plugins taskBackendSource, log *slog.Logger) (core.TaskBackend, string, error) {
	if cfg.IsPluginBackend() {
		// OF_SANDBOX_BACKEND names a plugin. It must serve the TaskBackend service.
		if plugins == nil {
			return nil, "", fmt.Errorf("OF_SANDBOX_BACKEND %q names a plugin, but no plugin host is available", cfg.SandboxBackend)
		}
		b, err := plugins.TaskBackend(cfg.SandboxBackend)
		if err != nil {
			return nil, "", err
		}
		image := cfg.SandboxImage
		if image == "" {
			image = kubernetes.DefaultImage
		}
		return b, image, nil
	}
	if cfg.SandboxBackend == config.BackendKubernetes {
		var tpl string
		if cfg.K8sPodTemplate != "" {
			b, err := os.ReadFile(cfg.K8sPodTemplate)
			if err != nil {
				return nil, "", fmt.Errorf("read OF_K8S_POD_TEMPLATE: %w", err)
			}
			tpl = string(b)
		}
		image := cfg.SandboxImage
		if image == "" {
			image = kubernetes.DefaultImage
		}
		runAsUser, runAsImage := cfg.K8sRunAsUserValue()
		b, err := k8stask.New(k8stask.Options{
			Kubeconfig:          cfg.Kubeconfig,
			Namespace:           cfg.K8sNamespace,
			PodTemplate:         tpl,
			RunnerImage:         cfg.RunnerImage,
			PreflightToolImages: cfg.PreflightToolImages(),
			ImagePullPolicy:     cfg.K8sImagePullPolicyValue(),
			ExtraLabels:         cfg.K8sLabelMap(),
			ExtraAnnotations:    cfg.K8sAnnotationMap(),
			RunAsUser:           runAsUser,
			RunAsImageUser:      runAsImage,

			WorkspaceSizeLimit:           cfg.K8sWorkspaceSizeLimit,
			ControlPlaneNamespace:        cfg.K8sControlPlaneNamespace,
			ControlPlaneSelector:         cfg.K8sControlPlaneSelector,
			EgressProxySidecarImage:      cfg.K8sEgressProxySidecarImage,
			EgressLock:                   cfg.K8sEgressLockValue(),
			NetworkPolicyCheck:           cfg.K8sNetworkPolicyCheckValue(),
			NetworkPolicyEnforcement:     cfg.K8sNetworkPolicyEnforcementValue(),
			EgressWaitTimeout:            cfg.K8sEgressWaitTimeoutValue(),
			EgressWaitAddr:               strings.TrimSpace(cfg.K8sEgressWaitAddr),
			AllowUnenforcedNetworkPolicy: cfg.AllowUnenforcedNetworkPolicy,
			PreflightJob:                 cfg.K8sPreflightJob,
			PreflightImage:               image,
			Logger:                       log,
		})
		if err != nil {
			return nil, "", err
		}
		return b, image, nil
	}
	command := strings.Fields(cfg.RunnerCommand)
	if len(command) == 0 {
		exe, err := os.Executable()
		if err != nil {
			return nil, "", fmt.Errorf("find the runner command: %w", err)
		}
		command = []string{exe, "runner"}
	}
	root := cfg.SandboxRoot
	if root == "" && cfg.DataDir != "" {
		root = filepath.Join(cfg.DataDir, "sandboxes")
	}
	extra := map[string]string{}
	if cfg.RunnerExecCommand != "" {
		extra["OF_RUNNER_EXEC_COMMAND"] = cfg.RunnerExecCommand
	}
	b, err := tasklocal.New(tasklocal.Options{
		Root:     filepath.Join(root, "tasks"),
		Command:  command,
		ExtraEnv: extra,
	})
	return b, "", err
}

// taskImageFor returns the function that picks the image of the task of a run
// on the kubernetes backend. It uses sandbox.SelectImage: the image of the environment of the run, then the
// image of the machine of the agent. An empty result means the default image.
// A failed lookup of the environment is logged and gives the default image.
func taskImageFor(st core.Store, log *slog.Logger) func(*core.Run) string {
	return func(run *core.Run) string {
		var spec core.SandboxSpec
		if run.Config.Machine != nil {
			spec.Machine = *run.Config.Machine
		}
		if id := run.Config.EnvironmentID; id != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			env, err := st.GetEnvironment(ctx, id)
			if err != nil {
				log.Warn("sandbox image: read environment failed, using the next image", "run", run.ID, "environment", id, "err", err)
			} else {
				spec.Environment = env
			}
		}
		return sandbox.SelectImage(spec, "")
	}
}

// toolImagesFor returns the function that picks the tool images of the task of
// a run: the image with the CLI of the harness of the run (claude or codex).
// Another harness needs none. An empty setting gives no tool image, so the
// task image must hold the CLI.
func toolImagesFor(cfg *config.Config) func(*core.Run) []core.TaskImage {
	return func(run *core.Run) []core.TaskImage {
		name := string(run.Config.Harness.Type)
		if image := cfg.ToolImageFor(name); image != "" {
			return []core.TaskImage{{Name: name, Image: image}}
		}
		return nil
	}
}

func newRunnerMode(cfg *config.Config, signKey []byte, signPrev [][]byte, st core.Store, bus core.EventBus, runData *objectstore.RunData, cloud runnerapi.CloudTokenSource, audit auth.AuditSink, plugins taskBackendSource, log *slog.Logger) (*runnerMode, error) {
	rs, ok := st.(core.RunnerStore)
	if !ok {
		return nil, errors.New("the store does not support the runner path (core.RunnerStore)")
	}
	keys, err := runnerKeySet(signKey, signPrev)
	if err != nil {
		return nil, fmt.Errorf("runner token keys: %w", err)
	}
	backend, image, err := buildTaskBackend(cfg, plugins, log)
	if err != nil {
		return nil, fmt.Errorf("sandbox backend: %w", err)
	}
	uploads := &uploadStorer{runData: runData} // keyOf is set when the runner API exists
	var onUpload func(context.Context, string, int, string, string, string) error
	if runData != nil {
		onUpload = uploads.store
	}
	var imageFor func(*core.Run) string
	if cfg.SandboxBackend == config.BackendKubernetes || cfg.IsPluginBackend() {
		imageFor = taskImageFor(st, log)
		// The preflight checks the permissions of the cluster account and,
		// with OF_K8S_PREFLIGHT_JOB, runs a test Job. A failure stops the start.
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		err := backend.Preflight(pctx)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("sandbox backend preflight: %w", err)
		}
	}
	router := &lateRouter{}
	// contentRef is set below, before the dispatcher starts.
	var contentRef *runnerContent
	d, err := dispatch.New(dispatch.Config{
		Preflight: func(ctx context.Context, run *core.Run) error {
			if contentRef == nil {
				return nil
			}
			x, err := contentRef.planner()
			if err != nil {
				return err // not ready: the error leaves the run queued, the check runs again
			}
			return x.CredentialPreflight(ctx, run)
		},
		Router:           router,
		PublicControlURL: cfg.PublicURL,
		ReplicaID:        coord.ReplicaID(cfg.ReplicaID),
		Store:            st,
		Runner:           rs,
		Backend:          backend,
		Image:            image,
		ImageFor:         imageFor,
		ToolImagesFor:    toolImagesFor(cfg),
		ControlURL:       cfg.SandboxURL(),
		Logger:           log,
		// Execution limits (OF_QUEUE_TIMEOUT and the settings next to it).
		QueueTimeout:      cfg.QueueTimeoutDuration(),
		BootstrapTTL:      cfg.SandboxStartTimeoutDuration(),
		MaxSessionSeconds: int(cfg.MaxSessionDurationValue().Seconds()),
		StopGrace:         cfg.SessionStopGraceDuration(),
		MaxSandboxes:      cfg.MaxSandboxes,
		Spend:             newSpendTracker(cfg, st, bus, log),
		FactoryLimit: func(ctx context.Context, factoryID string) (int, error) {
			rt, err := engine.LoadFactoryRuntime(ctx, st, factoryID)
			if err != nil {
				return 0, err
			}
			return rt.MaxConcurrentRuns, nil
		},
		OnRunChanged: func(ctx context.Context, r *core.Run) {
			cp := *r
			bus.Publish(ctx, core.TopicRunUpdated, &cp)
		},
		Queue:    queueOf(bus),
		OnUpload: onUpload,
		RequiredHosts: func(run *core.Run) []string {
			opts := engine.RequiredHostsOptions{SandboxURL: cfg.SandboxURL(), DefaultImage: cfg.SandboxImage}
			if id := run.Config.EnvironmentID; id != "" {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if env, err := st.GetEnvironment(ctx, id); err == nil {
					opts.Environment = env
				}
			}
			return engine.RequiredHosts(run.Config, opts)
		},
		OnRunMessage: func(ctx context.Context, m *core.RunMessage) {
			cp := *m
			bus.Publish(ctx, core.TopicRunMessage, &cp)
		},
	})
	if err != nil {
		return nil, err
	}
	content := &runnerContent{execHarness: cfg.RunnerExecCommand != "", snapshotMaxBytes: cfg.SnapshotMaxBytes, log: log,
		idleDefault: cfg.IdleTimeoutDefaultDuration(), idleMax: cfg.IdleTimeoutMaxDuration(),
		maxSessionSec: int(cfg.MaxSessionDurationValue().Seconds()), runner: rs}
	contentRef = content
	if audit == nil {
		// The same default as the auth service.
		audit = auth.SlogAuditSink{Logger: log}
	}
	content.audit = audit
	// The attachments are in the key/value store (metadata) and in the artifacts
	// store of the run data (files). The API writes them.
	lookupAttachment := func(ctx context.Context, id string) (core.Attachment, error) {
		raw, err := st.KVGet(ctx, core.AttachmentKVKey(id))
		if err != nil {
			return core.Attachment{}, err
		}
		return core.UnmarshalAttachment(raw)
	}
	d.SetAttachmentLookup(lookupAttachment)
	rc := runnerapi.Config{
		Attachments:    lookupAttachment,
		Store:          st,
		Runner:         rs,
		Keys:           keys,
		Session:        content,
		Secrets:        content,
		GitCredentials: content,
		CloudTokens:    cloud,
		Hooks:          d,
		Logger:         log,
		Redact:         content.redactFor,
	}
	if runData != nil {
		// Artifacts and transcripts of the runner share one store. Snapshots
		// use the snapshots store of the run data.
		putHeaders := func(fs objectstore.Store) func(string) map[string]string {
			return func(ct string) map[string]string {
				h := map[string]string{}
				for k, v := range objectstore.PutHeaders(fs, ct) {
					if len(v) > 0 {
						h[k] = v[0]
					}
				}
				return h
			}
		}
		if fs := runData.Store(objectstore.Artifacts); fs != nil {
			rc.Files = fs
			rc.AttachmentFiles = fs
			rc.PutHeaders = putHeaders(fs)
		}
		if fs := runData.Store(objectstore.Snapshots); fs != nil {
			rc.SnapshotFiles = fs
			rc.SnapshotPutHeaders = putHeaders(fs)
		}
		rc.SnapshotMaxBytes = cfg.SnapshotMaxBytes
	}
	svc, err := runnerapi.New(rc)
	if err != nil {
		return nil, err
	}
	uploads.keyOf = svc.ObjectKey
	d.SetCommander(svc)
	return &runnerMode{dispatcher: d, service: svc, backend: backend, content: content, keys: keys, store: rs, router: router, ownsTasks: backendOwnsTasks(cfg)}, nil
}

// route returns the part of the dispatcher that the engine uses.
func (m *runnerMode) route() engine.RunnerRoute { return m.dispatcher }

// verifierParts returns the session token keys and the store.
func (m *runnerMode) verifierParts() (runnertoken.KeySet, core.RunnerStore) {
	return m.keys, m.store
}

// loop calls the dispatcher until ctx ends. At the end it gives the dispatch
// lease up, so that a new replica claims runs at once after a rolling restart,
// and does not wait for the lease to end.
func (m *runnerMode) loop(ctx context.Context, every time.Duration, log *slog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if err := m.dispatcher.Tick(ctx); err != nil && ctx.Err() == nil {
			log.Error("dispatcher tick", "err", err)
		}
		select {
		case <-ctx.Done():
			rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			m.dispatcher.ReleaseLeadership(rctx)
			return
		case <-t.C:
		}
	}
}

// backendOwnsTasks reports whether the tasks of the configured backend are
// children of this process. That holds for the local backend only.
func backendOwnsTasks(cfg *config.Config) bool {
	return cfg.SandboxBackend != config.BackendKubernetes && !cfg.IsPluginBackend()
}

// stopTasks destroys the tasks that are still alive when the tasks run inside
// this process. With the kubernetes backend and with plugin backends the tasks
// keep running: another replica, or this one after a restart, still serves
// their sessions, and the list holds the tasks of every replica.
func (m *runnerMode) stopTasks(log *slog.Logger) {
	if !m.ownsTasks {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := m.backend.List(ctx)
	if err != nil {
		log.Warn("list runner tasks", "err", err)
		return
	}
	for _, st := range list {
		if err := m.backend.Destroy(ctx, st.TaskID); err != nil {
			log.Warn("destroy runner task", "task", st.TaskID, "err", err)
		}
	}
}

// runnerContent gives the runner API the content of a session. The planner
// computes the content.
type runnerContent struct {
	execHarness bool // development and tests only
	// snapshotMaxBytes is the limit of one snapshot part. 0 means no limit.
	snapshotMaxBytes int64
	// idleDefault is the idle timeout of an agent that sets none. idleMax
	// caps every idle timeout. maxSessionSec is the maximum session time.
	idleDefault, idleMax time.Duration
	maxSessionSec        int
	log                  *slog.Logger
	audit                auth.AuditSink                 // records secrets reads; nil disables
	runner               core.RunnerStore               // gets the secrets.delivered timeline event; nil disables
	plan                 atomic.Pointer[engine.Planner] // set after the planner is built
}

var (
	_ runnerapi.SessionConfigurer     = (*runnerContent)(nil)
	_ runnerapi.SecretsProvider       = (*runnerContent)(nil)
	_ runnerapi.GitCredentialProvider = (*runnerContent)(nil)
)

func (c *runnerContent) planner() (*engine.Planner, error) {
	x := c.plan.Load()
	if x == nil {
		return nil, errors.New("the session planner is not ready")
	}
	return x, nil
}

// setPlanner gives the content provider the planner.
func (m *runnerMode) setPlanner(x *engine.Planner) { m.content.plan.Store(x) }

// redactFor returns a function that removes the secret values of a run from
// text. It returns nil before the planner is ready.
func (c *runnerContent) redactFor(ctx context.Context, run *core.Run) func(string) string {
	x, err := c.planner()
	if err != nil {
		return nil
	}
	return x.RedactFor(ctx, run)
}

// StartConfig implements runnerapi.SessionConfigurer.
func (c *runnerContent) StartConfig(ctx context.Context, run *core.Run, _ *core.RunSession) (runnerproto.StartResponse, error) {
	x, err := c.planner()
	if err != nil {
		return runnerproto.StartResponse{}, err
	}
	plan, err := x.RunnerPlan(ctx, run)
	if err != nil {
		return runnerproto.StartResponse{}, fmt.Errorf("session plan: %w", err)
	}
	cfg := run.Config
	h := runnerproto.Harness{
		Type:            string(cfg.Harness.Type),
		Model:           cfg.Harness.Model,
		ReasoningLevel:  cfg.Harness.ReasoningLevel,
		SystemPrompt:    plan.SystemPrompt,
		ResumeSessionID: run.SessionID,
		MCPServers:      map[string]runnerproto.MCPServer{},
	}
	if c.execHarness {
		h.Type = "exec"
	}
	// The headers of a URL server go through the secrets request, not here.
	for name, srv := range plan.MCPServers {
		switch {
		case srv.URL != "" && plan.SessionTokenServers[name]:
			// The runner sends its session token to this server.
			h.MCPServers[name] = runnerproto.MCPServer{URL: srv.URL, Auth: "session_token"}
		case srv.URL != "":
			h.MCPServers[name] = runnerproto.MCPServer{URL: srv.URL}
		case srv.Command != "":
			// The env goes in the secrets response (MCPEnv).
			h.MCPServers[name] = runnerproto.MCPServer{Command: srv.Command, Args: srv.Args}
		}
	}
	ws := runnerproto.Workspace{
		Repositories:        []runnerproto.Repository{},
		SetupCommands:       []runnerproto.SetupCommand{},
		SetupTimeoutSeconds: runnerSetupTimeoutSec,
		Skills:              []runnerproto.Skill{},
		Snapshot: runnerproto.SnapshotConfig{
			Restore: true, Enabled: true,
			SkipWorkspace: cfg.Snapshot != nil && !*cfg.Snapshot,
			MaxPartBytes:  c.snapshotMaxBytes,
		},
	}
	for _, r := range plan.Repos {
		ws.Repositories = append(ws.Repositories, runnerproto.Repository{Forge: r.Forge, CloneURL: r.CloneURL, Path: r.Path, Ref: r.Ref})
	}
	for _, cmd := range plan.SetupCommands {
		ws.SetupCommands = append(ws.SetupCommands, runnerproto.SetupCommand{Origin: "machine", Command: cmd})
	}
	for _, cmd := range plan.EnvSetupCommands {
		ws.SetupCommands = append(ws.SetupCommands, runnerproto.SetupCommand{Origin: "environment", Command: cmd})
	}
	for _, sk := range plan.Skills {
		files := map[string]string{}
		for k, v := range sk.Files {
			files[k] = v
		}
		files["SKILL.md"] = sk.Content
		ws.Skills = append(ws.Skills, runnerproto.Skill{Name: sk.Name, Files: files})
	}
	env := plan.Env
	if env == nil {
		env = map[string]string{}
	}
	if plan.WorkDir != "" {
		// The runner will start the harness in this directory later.
		env["OF_WORKDIR"] = plan.WorkDir
	}
	return runnerproto.StartResponse{
		ConversationID:     run.ID,
		Workspace:          ws,
		Harness:            h,
		Env:                env,
		IdleTimeoutSeconds: c.idleTimeoutSeconds(cfg.IdleTimeoutMinutes, cfg.IdleTimeoutSet),
		IdleTimeoutSet:     true,
		Limits:             runnerproto.Limits{MaxSessionSeconds: c.sessionLimitSeconds(), EventBatchBytes: runnerEventBatchBytes},
		Cloud:              runnerCloud(plan.Cloud, run.ID),
	}, nil
}

// runnerCloud converts the cloud identity of a plan for the start response.
func runnerCloud(s *cloudid.Setup, runID string) *runnerproto.CloudIdentity {
	if s == nil {
		return nil
	}
	out := &runnerproto.CloudIdentity{}
	if s.AWS != nil {
		out.AWS = &runnerproto.CloudAWS{RoleARN: s.AWS.RoleARN, Audience: s.AWS.Audience, SessionName: cloudid.SessionName(runID)}
	}
	if s.GCP != nil {
		out.GCP = &runnerproto.CloudGCP{Audience: s.GCP.Audience, ProjectNumber: s.GCP.ProjectNumber, ServiceAccountEmail: s.GCP.ServiceAccountEmail}
	}
	return out
}

// Secrets implements runnerapi.SecretsProvider.
func (c *runnerContent) Secrets(ctx context.Context, run *core.Run, s *core.RunSession) (runnerproto.SecretsResponse, error) {
	x, err := c.planner()
	if err != nil {
		return runnerproto.SecretsResponse{}, err
	}
	plan, err := x.RunnerPlan(ctx, run)
	if err != nil {
		return runnerproto.SecretsResponse{}, fmt.Errorf("session plan: %w", err)
	}
	resp := runnerproto.SecretsResponse{
		Env:         plan.SecretEnv,
		Files:       []runnerproto.SecretFile{},
		HarnessAuth: plan.HarnessAuth,
		MCPHeaders:  map[string]map[string]string{},
		Redact:      []string{},
	}
	if resp.Env == nil {
		resp.Env = map[string]string{}
	}
	if resp.HarnessAuth == nil {
		resp.HarnessAuth = map[string]string{}
	}
	var values []string
	for _, v := range plan.SecretEnv {
		values = append(values, v)
	}
	for _, v := range plan.HarnessAuth {
		values = append(values, v)
	}
	for name, srv := range plan.MCPServers {
		// The env of a command server can hold expanded secrets, so it
		// goes here and not in the start response.
		if srv.Command != "" && len(srv.Env) > 0 {
			if resp.MCPEnv == nil {
				resp.MCPEnv = map[string]map[string]string{}
			}
			resp.MCPEnv[name] = srv.Env
			for _, v := range srv.Env {
				values = append(values, v)
			}
		}
		if srv.URL == "" || len(srv.Headers) == 0 || plan.SessionTokenServers[name] {
			continue
		}
		resp.MCPHeaders[name] = srv.Headers
		for _, v := range srv.Headers {
			values = append(values, v)
			if tok, ok := strings.CutPrefix(v, "Bearer "); ok {
				values = append(values, tok)
			}
		}
	}
	resp.Redact = redactList(values)
	if c.audit != nil {
		c.audit.Record(ctx, secretsReadEvent(run, s, resp))
	}
	c.recordSecretsDelivered(ctx, run, s, resp)
	return resp, nil
}

// recordSecretsDelivered writes the timeline event secrets.delivered. It
// holds the names of the secrets and never their values. The event ID holds
// the task ID, so a second read of the same session adds no row.
func (c *runnerContent) recordSecretsDelivered(ctx context.Context, run *core.Run, s *core.RunSession, resp runnerproto.SecretsResponse) {
	if c.runner == nil || s == nil {
		return
	}
	names := map[string]bool{}
	for n := range resp.Env {
		names[n] = true
	}
	for n := range resp.HarnessAuth {
		names[n] = true
	}
	for _, f := range resp.Files {
		names[f.Path] = true
	}
	data, err := json.Marshal(map[string]any{"names": sortedKeys(names)})
	if err != nil {
		return
	}
	ev := &core.TimelineEvent{EventID: "secrets.delivered:" + s.TaskID, RunID: run.ID, Session: s.Session,
		Type: "secrets.delivered", At: time.Now().UTC(), Data: data}
	if _, err := c.runner.AppendTimelineEvent(ctx, ev); err != nil {
		c.log.Warn("write timeline", "run", run.ID, "type", ev.Type, "err", err)
	}
}

// secretsReadEvent builds the audit event of a secrets read. It holds names
// and never values.
func secretsReadEvent(run *core.Run, s *core.RunSession, resp runnerproto.SecretsResponse) auth.AuditEvent {
	files := make([]string, 0, len(resp.Files))
	for _, f := range resp.Files {
		files = append(files, f.Path)
	}
	sort.Strings(files)
	servers := map[string]bool{}
	for n := range resp.MCPHeaders {
		servers[n] = true
	}
	for n := range resp.MCPEnv {
		servers[n] = true
	}
	session := 0
	if s != nil {
		session = s.Session
	}
	return auth.AuditEvent{
		Action:   "runner.secrets.read",
		Outcome:  auth.OutcomeSuccess,
		ActorID:  "run:" + run.ID,
		TeamID:   run.TeamID,
		Resource: &auth.Resource{Type: "run", ID: run.ID, TeamID: run.TeamID},
		Detail: map[string]string{
			"run_id":       run.ID,
			"session":      strconv.Itoa(session),
			"team_id":      run.TeamID,
			"env":          strings.Join(sortedKeys(resp.Env), ","),
			"files":        strings.Join(files, ","),
			"harness_auth": strings.Join(sortedKeys(resp.HarnessAuth), ","),
			"mcp_servers":  strings.Join(sortedKeys(servers), ","),
		},
	}
}

// GitCredential implements runnerapi.GitCredentialProvider.
func (c *runnerContent) GitCredential(ctx context.Context, run *core.Run, _ *core.RunSession, req runnerproto.GitCredentialRequest) (runnerproto.GitCredentialResponse, error) {
	x, err := c.planner()
	if err != nil {
		return runnerproto.GitCredentialResponse{}, err
	}
	user, pw, exp, err := x.RunnerGitCredential(ctx, run, req.Host, req.Path)
	if err != nil {
		return runnerproto.GitCredentialResponse{}, err
	}
	c.log.Info("runner git credential", "run_id", run.ID, "host", req.Host, "path", req.Path)
	return runnerproto.GitCredentialResponse{Username: user, Password: pw, ExpiresAt: exp}, nil
}

// redactList returns the distinct secret values and, for a value of 8 or
// more characters, its standard base64 form.
func redactList(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	for _, v := range values {
		add(v)
		if len(v) >= 8 {
			add(base64.StdEncoding.EncodeToString([]byte(v)))
		}
	}
	sort.Strings(out)
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// idleTimeoutSeconds returns the idle timeout of a session. The agent value
// (minutes) wins when the agent set one, also 0, which ends the session after
// the turn. Else the server default applies. The result never passes the
// server maximum.
func (c *runnerContent) idleTimeoutSeconds(agentMinutes int, set bool) int {
	d := c.idleDefault
	if set || agentMinutes > 0 {
		d = time.Duration(agentMinutes) * time.Minute
	}
	if d > c.idleMax {
		d = c.idleMax
	}
	return int(d.Seconds())
}

// sessionLimitSeconds returns the maximum session time that the runner gets.
func (c *runnerContent) sessionLimitSeconds() int {
	if c.maxSessionSec > 0 {
		return c.maxSessionSec
	}
	return runnerMaxSessionSec
}

// queueOf returns bus as a core.Queue, or nil when it is not one.
func queueOf(bus core.EventBus) core.Queue {
	if q, ok := bus.(core.Queue); ok {
		return q
	}
	return nil
}
