// Package runner is the process that runs inside a sandbox. It prepares the
// workspace, runs the harness and talks to the control plane over HTTPS.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netlock"
)

const (
	defaultVersion     = "0.0.0-dev"
	defaultPollWait    = 25
	defaultIdleTimeout = 600
	startRetryTotal    = 2 * time.Minute
	endTimeout         = 2 * time.Minute
	gracefulEndTimeout = 60 * time.Second
	tokenRefreshBefore = 10 * time.Minute
	minDelay           = time.Second
	maxDelay           = 30 * time.Second
	maxEndGapTries     = 5
)

// errSuperseded is the cause of the session context when a newer session exists.
var errSuperseded = errors.New("runner: session superseded")

// Config is the configuration of one session.
type Config struct {
	ControlURL     string
	RunID          string // for logs only; the start response is the truth
	Session        int    // for logs only
	BootstrapToken string
	WorkDir        string             // the workspace; created if missing
	StateDir       string             // event log, session file and snapshot files; created if missing; must not be inside WorkDir
	Harnesses      map[string]Harness // key: StartResponse.Harness.Type
	Version        string             // sent as runner_version and in the version header; empty means "0.0.0-dev"
	SandboxID      string
	HTTPClient     *http.Client // nil means a default client
	Logger         *slog.Logger // nil means slog.Default()
	PollWait       int          // seconds for the commands long poll; 0 means 25
	// GitCredentialHelper is the value of git's credential.helper for clones.
	// Empty means no helper.
	GitCredentialHelper string
	// CloudTokenCommand is the shell command of the cloud token helper,
	// without --audience. The GCP credential file runs it. Empty means no
	// command; the runner then writes token files and refreshes them.
	CloudTokenCommand string
	// HomeDir is the home directory of the task user; empty means os.UserHomeDir().
	HomeDir string
	// SnapshotDeclarations is the path from OF_SNAPSHOT_DECLARATIONS; empty means none.
	SnapshotDeclarations string
	// EgressReportURL is the URL of the report of blocked hosts of the egress
	// proxy (OF_NETGUARD_REPORT). Empty means no report.
	EgressReportURL string
	// EgressLock is the in-sandbox lock mode (OF_EGRESS_LOCK). With a mode
	// other than none, the runner must run as root: it locks the network and
	// runs every user-controlled process as the agent user.
	EgressLock netlock.Mode
	// LoopbackDaemonPorts are the loopback ports of provider daemons that the
	// agent must not reach (OF_LOOPBACK_DAEMON_PORTS).
	LoopbackDaemonPorts []int
	// Executable is the path of the runtime binary. The lock runs netguard
	// and its probe with it, and the credential helpers use it.
	Executable string
	// Locker sets up the lock. Nil means the real system.
	Locker *netlock.Locker
	// GitMirrorDir holds read-only git mirrors named <key>.git (see
	// GitCacheKey). A clone uses a mirror as a reference when it exists.
	// Empty means DefaultGitMirrorDir.
	GitMirrorDir string
	Now          func() time.Time // nil means time.Now
}

// DefaultGitMirrorDir is where a microVM worker mounts the git mirrors of a session.
const DefaultGitMirrorDir = "/cache/git"

// ErrRunnerVersionMismatch means that the control plane serves another
// runner version. The runner exits with ExitVersionMismatch.
var ErrRunnerVersionMismatch = errors.New("runner: runner version mismatch")

// ExitVersionMismatch is the exit code for ErrRunnerVersionMismatch. The
// provider maps it to the reason invalid_spec.
const ExitVersionMismatch = 65

// instanceIDBytes is the size of the random part of an instance ID.
const instanceIDBytes = 16

// NewInstanceID returns a new random instance ID for the session start
// request: 16 random bytes in base64url without padding.
func NewInstanceID() (string, error) {
	var b [instanceIDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// runner holds the state of one session.
type runner struct {
	cfg    Config
	log    *slog.Logger
	client *runnerclient.Client

	start     runnerproto.StartResponse
	secretEnv map[string]string
	redact    []string
	// The next three values come from the secrets response. They stay in memory.
	harnessAuth map[string]string
	mcpHeaders  map[string]map[string]string
	mcpEnv      map[string]map[string]string
	secretFiles []runnerproto.SecretFile
	// cloudEnv holds the variables of the cloud credential files, and
	// cloudFiles the token files that the runner refreshes (file -> audience).
	cloudEnv   map[string]string
	cloudFiles map[string]string
	harness    Harness
	mcpForward *mcpForwarder // nil if no server takes the session token

	turnDir     string      // work directory of the turns
	ready       bool        // setup is complete
	repos       []repoState // the clones, in the order of the start response
	restoreNote string      // text for the first turn; empty after use
	// harnessStateRestored is true when the restore unpacked a harness_state
	// part, so the harness can resume its stored session.
	harnessStateRestored bool
	// harnessSessionID is the newest harness session ID from a harness.session event.
	sessionMu        sync.Mutex
	harnessSessionID string
	// egressMu guards egressSeen, the hosts that the runner reported as blocked.
	egressMu   sync.Mutex
	egressSeen map[string]bool
	// conversationDone is true after withConversation ran once.
	conversationDone bool

	events  *runnerclient.EventLog
	sender  *runnerclient.Sender
	flushCh chan struct{}

	flushCtx    context.Context
	flushStop   context.CancelFunc
	flusherDone chan struct{}

	cancelRun context.CancelCauseFunc

	// lock is the egress lock; nil when the lock is off.
	lock *netlock.Lock
	// agent is the user of the user-controlled processes; nil when the lock is off.
	agent *netlock.Credential
	// helperSrv serves the credential helpers when the lock is on.
	helperSrv *http.Server
	helperURL string

	arts          artifactState // artifacts uploaded in this session
	transcriptSum [32]byte      // hash of the last raw transcript that was uploaded
}

func (r *runner) now() time.Time { return r.cfg.Now() }

// markSuperseded stops the session because a newer session exists.
func (r *runner) markSuperseded() {
	if r.cancelRun != nil {
		r.cancelRun(errSuperseded)
	}
}

// Run runs one session. It returns nil when the session ended with a
// session end call that succeeded, or when the control plane said that a
// newer session exists (superseded). It returns an error for other failures.
// When ctx is done, Run ends the session with the reason "stopped" (a
// graceful stop) and returns.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.prepare(); err != nil {
		return err
	}
	r := &runner{cfg: cfg, log: cfg.Logger, flushCh: make(chan struct{}, 1)}
	instance, err := NewInstanceID()
	if err != nil {
		return fmt.Errorf("runner: make instance id: %w", err)
	}
	r.client, err = runnerclient.New(runnerclient.Options{BaseURL: cfg.ControlURL, HTTPClient: cfg.HTTPClient, RunnerVersion: cfg.Version})
	if err != nil {
		return err
	}
	startedAt := r.now()

	// Session start and secrets.
	req := runnerproto.StartRequest{
		InstanceID:    instance,
		RunnerVersion: cfg.Version,
		SandboxID:     cfg.SandboxID,
		Platform:      runnerproto.Platform{OS: runtime.GOOS, Arch: runtime.GOARCH},
	}
	err = retryStartup(ctx, func() (err error) {
		r.start, err = r.client.Start(ctx, cfg.BootstrapToken, req)
		return err
	})
	if err != nil {
		if runnerclient.IsVersionMismatch(err) {
			return fmt.Errorf("%w: %w", ErrRunnerVersionMismatch, err)
		}
		return startupResult(err, "session start")
	}
	if want := r.start.RunnerVersion; want != "" && want != cfg.Version {
		return fmt.Errorf("%w: the control plane serves %s, this runner is %s", ErrRunnerVersionMismatch, want, cfg.Version)
	}
	r.log = r.log.With("run_id", r.start.RunID, "session", r.start.Session)
	r.keepSession()
	var secrets runnerproto.SecretsResponse
	err = retryStartup(ctx, func() (err error) {
		secrets, err = r.client.Secrets(ctx)
		return err
	})
	if err != nil {
		return startupResult(err, "secrets")
	}
	r.secretEnv = secrets.Env
	r.redact = secrets.Redact
	r.harnessAuth = secrets.HarnessAuth
	r.mcpHeaders = secrets.MCPHeaders
	r.mcpEnv = secrets.MCPEnv
	r.secretFiles = secrets.Files

	// The egress lock comes before any agent code, the clones included.
	if err := r.applyLock(ctx); err != nil {
		return err
	}
	defer r.closeLock()

	// The event log and the flusher.
	r.events, err = runnerclient.OpenEventLog(filepath.Join(cfg.StateDir, "events.jsonl"), cfg.Now)
	if err != nil {
		return err
	}
	defer r.events.Close()
	r.sender, err = runnerclient.NewSender(runnerclient.SenderOptions{Client: r.client, Log: r.events, BatchBytes: r.start.Limits.EventBatchBytes})
	if err != nil {
		return err
	}
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	r.cancelRun = cancel
	r.flushCtx, r.flushStop = context.WithCancel(rctx)
	r.flusherDone = make(chan struct{})
	go r.flusher()
	defer r.stopFlusher()
	if secrets.HarnessAuthDefault {
		r.dropDefaultAuth()
	}

	// The local forwarder adds the session token to the MCP requests.
	r.startMCPForward()
	defer func() { r.mcpForward.Close() }()

	// Workspace setup.
	if err := r.setup(rctx); err != nil {
		if info, stop := r.interrupted(rctx); stop {
			return r.finish(info)
		}
		var se *setupError
		if !errors.As(err, &se) {
			se = &setupError{err.Error()}
		}
		return r.finish(endInfo{reason: runnerproto.EndSetupFailed, code: string(rtcore.FailureEnvironmentSetupFailed), msg: se.msg, timeout: endTimeout})
	}
	h, ok := cfg.Harnesses[r.start.Harness.Type]
	if !ok {
		return r.finish(endInfo{reason: runnerproto.EndSetupFailed, code: string(rtcore.FailureEnvironmentSetupFailed), msg: "unknown harness type", timeout: endTimeout})
	}
	r.harness = h
	go r.refreshCloud(rctx)
	go r.watchEgress(rctx)
	r.turnDir = r.workDirOfTurns()
	r.ready = true
	ready := runnerproto.SessionReadyData{Capabilities: h.Capabilities()}
	for _, st := range r.repos {
		if st.base != "" {
			ready.Repositories = append(ready.Repositories, runnerproto.RepositoryCommit{Path: st.path, Commit: st.base})
		}
	}
	r.emit(runnerproto.EventSessionReady, "", ready)

	return r.finish(r.commandLoop(rctx, startedAt))
}

// interrupted reports whether ctx is done and what to do about it.
func (r *runner) interrupted(ctx context.Context) (endInfo, bool) {
	if ctx.Err() == nil {
		return endInfo{}, false
	}
	if errors.Is(context.Cause(ctx), errSuperseded) {
		return endInfo{superseded: true}, true
	}
	return endInfo{reason: runnerproto.EndStopped, timeout: gracefulEndTimeout}, true
}

// prepare checks the configuration and fills in the defaults.
func (c *Config) prepare() error {
	switch {
	case c.ControlURL == "":
		return errors.New("runner: ControlURL is empty")
	case c.BootstrapToken == "":
		return errors.New("runner: BootstrapToken is empty")
	case c.WorkDir == "":
		return errors.New("runner: WorkDir is empty")
	case c.StateDir == "":
		return errors.New("runner: StateDir is empty")
	}
	wd, err := filepath.Abs(c.WorkDir)
	if err != nil {
		return err
	}
	sd, err := filepath.Abs(c.StateDir)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(wd, sd); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("runner: StateDir must not be inside WorkDir")
	}
	c.WorkDir, c.StateDir = wd, sd
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return fmt.Errorf("runner: create WorkDir: %w", err)
	}
	if err := os.MkdirAll(sd, 0o700); err != nil {
		return fmt.Errorf("runner: create StateDir: %w", err)
	}
	if c.Version == "" {
		c.Version = defaultVersion
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.PollWait <= 0 {
		c.PollWait = defaultPollWait
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.HomeDir == "" {
		if h, err := os.UserHomeDir(); err == nil {
			c.HomeDir = h
		}
	}
	return nil
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func nextDelay(d time.Duration) time.Duration {
	if d *= 2; d > maxDelay {
		return maxDelay
	}
	return d
}

// retryStartup calls fn again after a retryable error. The delay starts at
// 1 s and doubles up to 30 s. It gives up after 2 minutes.
func retryStartup(ctx context.Context, fn func() error) error {
	deadline := time.Now().Add(startRetryTotal)
	delay := minDelay
	for {
		err := fn()
		if err == nil || !runnerclient.IsRetryable(err) || time.Now().Add(delay).After(deadline) {
			return err
		}
		if serr := sleepCtx(ctx, delay); serr != nil {
			return serr
		}
		delay = nextDelay(delay)
	}
}

// startupResult turns an error before the session is ready into the result of Run.
func startupResult(err error, what string) error {
	if runnerclient.IsRunCancelled(err) || runnerclient.IsSuperseded(err) {
		return nil
	}
	return fmt.Errorf("runner: %s: %w", what, err)
}
