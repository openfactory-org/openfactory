package vmnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// HelperSpec describes the per-VM network helper.
type HelperSpec struct {
	VM VMNet
	// Exe is the runtime binary.
	Exe string
	// UID runs the helper (jail.ofnet_uid).
	UID int
	// Allowed, Denied and AlwaysAllow are the netguard host lists of a
	// restricted task. AlwaysAllow holds the control-plane host.
	Allowed, Denied, AlwaysAllow []string
	// PrivateHosts may resolve to private addresses (internal_allow).
	PrivateHosts []string
	// Upstreams are the resolvers of the DNS relay (unrestricted).
	Upstreams []netip.Addr
}

// Args returns the command line of the helper. A restricted VM gets
// netguard on <host_ip>:3128 with the report on <host_ip>:3129; an
// unrestricted VM gets the DNS relay on <host_ip>:53.
func (s HelperSpec) Args() []string {
	host := s.VM.HostIP.String()
	if s.VM.Mode == Unrestricted {
		args := []string{s.Exe, "worker", "dns-relay", "--listen", net.JoinHostPort(host, strconv.Itoa(DNSPort))}
		for _, u := range s.Upstreams {
			args = append(args, "--upstream", u.String())
		}
		return args
	}
	args := []string{
		s.Exe, "netguard",
		"--listen", net.JoinHostPort(host, strconv.Itoa(ProxyPort)),
		"--report-listen", net.JoinHostPort(host, strconv.Itoa(ReportPort)),
	}
	add := func(flag string, vals []string) {
		if len(vals) > 0 {
			args = append(args, flag, strings.Join(vals, ","))
		}
	}
	add("--allow", s.Allowed)
	add("--deny", s.Denied)
	add("--always-allow", s.AlwaysAllow)
	add("--allow-private-host", s.PrivateHosts)
	return args
}

// ListenAddr returns the TCP address that shows the helper is ready.
func (s HelperSpec) ListenAddr() string {
	port := ProxyPort
	if s.VM.Mode == Unrestricted {
		port = DNSPort
	}
	return net.JoinHostPort(s.VM.HostIP.String(), strconv.Itoa(port))
}

// Helper runs the per-VM helper and restarts it when it exits.
type Helper struct {
	spec    HelperSpec
	log     *slog.Logger
	start   func(args []string, uid int) (*exec.Cmd, error)
	onFail  func(error)
	backoff time.Duration
	tries   int

	mu      sync.Mutex
	cmd     *exec.Cmd
	stopped bool
	// stopCh is closed by Stop. It ends the wait between restarts.
	stopCh chan struct{}
	done   chan struct{}
}

// HelperOptions configures StartHelper.
type HelperOptions struct {
	Logger *slog.Logger
	// OnFail is called when the helper exited Tries times in a row.
	OnFail func(error)
	// Start starts a process as uid. Nil means the default (setuid, and
	// CAP_NET_BIND_SERVICE on Linux).
	Start func(args []string, uid int) (*exec.Cmd, error)
	// Backoff between restarts (1 s) and Tries (5).
	Backoff time.Duration
	Tries   int
	// ReadyTimeout bounds the wait for the listener (5 s).
	ReadyTimeout time.Duration
}

// ErrHelperNotReady means the helper did not listen in time. For a
// restricted task this is network_policy_timeout.
var ErrHelperNotReady = errors.New("network helper did not listen in time")

// StartHelper starts the helper and waits until it listens.
func StartHelper(ctx context.Context, spec HelperSpec, o HelperOptions) (*Helper, error) {
	h := newHelper(spec, o)
	if err := h.launch(); err != nil {
		return nil, err
	}
	go h.supervise()
	timeout := o.ReadyTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if err := rtcore.WaitDial(ctx, "tcp", spec.ListenAddr(), timeout); err != nil {
		h.Stop()
		return nil, fmt.Errorf("%w: %s", ErrHelperNotReady, spec.ListenAddr())
	}
	return h, nil
}

func newHelper(spec HelperSpec, o HelperOptions) *Helper {
	h := &Helper{
		spec: spec, log: o.Logger, start: o.Start, onFail: o.OnFail, backoff: o.Backoff, tries: o.Tries,
		stopCh: make(chan struct{}), done: make(chan struct{}),
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.start == nil {
		h.start = startAs
	}
	if h.backoff <= 0 {
		h.backoff = time.Second
	}
	if h.tries <= 0 {
		h.tries = 5
	}
	return h
}

// errHelperStopped means Stop ran while a process started.
var errHelperStopped = errors.New("network helper stopped")

func (h *Helper) launch() error {
	cmd, err := h.start(h.spec.Args(), h.spec.UID)
	if err != nil {
		return fmt.Errorf("start network helper: %w", err)
	}
	h.mu.Lock()
	if h.stopped {
		// Stop ran while the process started: it must not outlive Stop.
		h.mu.Unlock()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		return errHelperStopped
	}
	h.cmd = cmd
	h.mu.Unlock()
	return nil
}

func (h *Helper) supervise() {
	defer close(h.done)
	fails := 0
	for {
		h.mu.Lock()
		cmd := h.cmd
		h.mu.Unlock()
		err := cmd.Wait()
		h.mu.Lock()
		stopped := h.stopped
		h.mu.Unlock()
		if stopped {
			return
		}
		fails++
		h.log.Warn("network helper exited", "tap", h.spec.VM.Tap, "err", err, "restart", fails)
		if fails >= h.tries {
			if h.onFail != nil {
				h.onFail(fmt.Errorf("network helper of %s exited %d times: %w", h.spec.VM.Tap, fails, err))
			}
			return
		}
		select {
		case <-h.stopCh:
			return
		case <-time.After(h.backoff):
		}
		if err := h.launch(); err != nil {
			if errors.Is(err, errHelperStopped) {
				return
			}
			if h.onFail != nil {
				h.onFail(err)
			}
			return
		}
	}
}

// Stop ends the helper and waits for it.
func (h *Helper) Stop() {
	h.mu.Lock()
	if !h.stopped {
		h.stopped = true
		close(h.stopCh)
	}
	cmd := h.cmd
	h.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
	}
}

// PID returns the process ID of the current helper process.
func (h *Helper) PID() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}
