package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
	"github.com/openfactory-org/openfactory/internal/runnerclient"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
	"github.com/openfactory-org/openfactory/internal/sandbox/netlock"
	"github.com/openfactory-org/openfactory/internal/snapshot"
)

// applyLock locks the network of the agent when the provider asks for it
// (Config.EgressLock). It returns an error that wraps netlock.ErrNotProven
// when the lock cannot be proven; the runner then exits with
// netlock.ExitCode and runs no agent code.
func (r *runner) applyLock(ctx context.Context) error {
	if !r.cfg.EgressLock.Enabled() {
		return nil
	}
	np := r.start.NetworkPolicy
	if np == nil {
		return fmt.Errorf("%w: the session start response has no network policy", netlock.ErrNotProven)
	}
	if r.start.EgressLock == runnerproto.EgressLockOff {
		return fmt.Errorf("%w: the control plane turned the lock off", netlock.ErrNotProven)
	}
	locker := r.cfg.Locker
	if locker == nil {
		locker = &netlock.Locker{}
	}
	l, err := locker.Setup(ctx, netlock.Params{
		Mode: r.cfg.EgressLock,
		Policy: rtcore.EffectiveNetworkPolicy{
			Restricted: np.Restricted, AllowedHosts: np.AllowedHosts, DeniedHosts: np.DeniedHosts,
		},
		ControlURL:          r.cfg.ControlURL,
		AlwaysAllow:         np.AlwaysAllow,
		LoopbackDaemonPorts: r.cfg.LoopbackDaemonPorts,
		Workspace:           r.cfg.WorkDir,
		AgentDirs:           []string{r.agentDir()},
		Executable:          r.cfg.Executable,
		Logger:              r.log,
	})
	if err != nil {
		return err
	}
	if l == nil {
		return nil
	}
	r.lock = l
	cred := l.Agent
	r.agent = &cred
	if cred.Home != "" {
		r.cfg.HomeDir = cred.Home
	}
	if r.cfg.EgressReportURL == "" {
		r.cfg.EgressReportURL = l.ReportURL
	}
	if err := r.startHelpers(); err != nil {
		l.Close()
		return fmt.Errorf("%w: start the credential helpers: %w", netlock.ErrNotProven, err)
	}
	return nil
}

// closeLock stops netguard and the helper endpoint.
func (r *runner) closeLock() {
	if r.helperSrv != nil {
		_ = r.helperSrv.Close()
	}
	r.lock.Close()
}

// agentDir is a directory that the agent owns, outside the workspace and
// outside the state directory: cloud files and snapshot staging go there
// when the lock is on.
func (r *runner) agentDir() string { return r.cfg.StateDir + ".agent" }

// scratchDir returns a staging directory for files that git or the agent
// reads or writes.
func (r *runner) scratchDir(name string) string {
	if r.agent != nil {
		return filepath.Join(r.agentDir(), name)
	}
	return filepath.Join(r.cfg.StateDir, name)
}

// cloudDir is the directory of the cloud credential files.
func (r *runner) cloudDir() string { return r.scratchDir("cloud") }

// asAgent makes cmd run as the agent when the lock is on.
func (r *runner) asAgent(cmd *exec.Cmd) {
	if r.agent != nil {
		r.agent.Apply(cmd)
	}
}

// giveToAgent gives a file tree to the agent when the lock is on.
func (r *runner) giveToAgent(path string) error {
	if r.agent == nil {
		return nil
	}
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return netlock.ChownTree(path, r.agent.UID, r.agent.GID)
}

// writeAgentFile writes a file atomically; with the lock on, the agent owns it.
func (r *runner) writeAgentFile(name string, data []byte, mode os.FileMode) error {
	if err := writeFileAtomic(name, data, mode); err != nil {
		return err
	}
	if r.agent != nil {
		return os.Lchown(name, r.agent.UID, r.agent.GID)
	}
	return nil
}

// withGitHook returns ctx with the agent hook for the git commands of the
// snapshot package.
func (r *runner) withGitHook(ctx context.Context) context.Context {
	if r.agent == nil {
		return ctx
	}
	return snapshot.WithCommandHook(ctx, r.asAgent)
}

// gitCredentialHelper is the value of git's credential.helper.
func (r *runner) gitCredentialHelper() string {
	if r.helperURL != "" && r.cfg.Executable != "" {
		return "!" + shellQuote(r.cfg.Executable) + " runner git-credential --endpoint " + shellQuote(r.helperURL)
	}
	return r.cfg.GitCredentialHelper
}

// cloudTokenCommand is the shell command of the cloud token helper, without
// --audience.
func (r *runner) cloudTokenCommand() string {
	if r.helperURL != "" && r.cfg.Executable != "" {
		return shellQuote(r.cfg.Executable) + " runner cloud-token --endpoint " + shellQuote(r.helperURL)
	}
	return r.cfg.CloudTokenCommand
}

// startHelpers serves the git credential and cloud token helpers on a
// loopback port. With the lock on, the agent runs git and the SDKs, and it
// cannot read the session file in the state directory of root.
func (r *runner) startHelpers() error {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /git-credential", func(w http.ResponseWriter, req *http.Request) {
		attrs, err := readCredentialInput(io.LimitReader(req.Body, 64<<10))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		resp, err := r.client.GitCredential(req.Context(), runnerproto.GitCredentialRequest{
			Protocol: attrs["protocol"], Host: attrs["host"], Path: attrs["path"],
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_, _ = fmt.Fprintf(w, "username=%s\npassword=%s\n", resp.Username, resp.Password)
	})
	mux.HandleFunc("GET /cloud-token", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		ttl, _ := strconv.Atoi(q.Get("ttl"))
		if err := writeCloudToken(req.Context(), r.client, q.Get("audience"), ttl, q.Get("format"), w); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
		}
	})
	r.helperSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	r.helperURL = "http://" + ln.Addr().String()
	go func() { _ = r.helperSrv.Serve(ln) }()
	return nil
}

// GitCredentialVia is the git credential helper that asks the helper
// endpoint of the runner (see the --endpoint flag).
func GitCredentialVia(ctx context.Context, endpoint, op string, in io.Reader, out io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(in, 64<<10))
	if err != nil {
		return err
	}
	if op != "get" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(endpoint, "/")+"/git-credential", strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	return helperCall(req, out)
}

// CloudTokenVia is the cloud token helper that asks the helper endpoint of
// the runner.
func CloudTokenVia(ctx context.Context, endpoint, audience string, ttlSeconds int, format string, out io.Writer) error {
	if err := checkCloudTokenArgs(audience, format); err != nil {
		return err
	}
	q := url.Values{"audience": {audience}, "ttl": {strconv.Itoa(ttlSeconds)}, "format": {format}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/cloud-token?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	return helperCall(req, out)
}

func helperCall(req *http.Request, out io.Writer) error {
	resp, err := (&http.Client{Timeout: time.Minute, Transport: &http.Transport{Proxy: nil}}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("runner helper: status %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// writeCloudToken asks the control plane for a token and writes it in format.
func writeCloudToken(ctx context.Context, c *runnerclient.Client, audience string, ttlSeconds int, format string, out io.Writer) error {
	if err := checkCloudTokenArgs(audience, format); err != nil {
		return err
	}
	resp, err := c.CloudToken(ctx, audience, ttlSeconds)
	if err != nil {
		return fmt.Errorf("cloud token: %w", err)
	}
	if format == CloudTokenFormatGCP {
		b, err := gcpExecutableResponse(resp.Token, resp.ExpiresAt)
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	}
	_, err = fmt.Fprint(out, resp.Token)
	return err
}
