package netlock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultDirectTargets are addresses that the agent must not reach directly:
// public resolvers by IP (IPv4 and IPv6) and the cloud metadata address.
var DefaultDirectTargets = []string{
	"1.1.1.1:443",
	"8.8.8.8:80",
	"[2606:4700:4700::1111]:443",
	"169.254.169.254:80",
}

// ProbeDeniedHost is a host that no policy allows. A restricted proxy must
// refuse it with 403.
const ProbeDeniedHost = "openfactory-lock-probe.invalid:443"

// ProbeSpec is the input of the probe. The runner passes it as JSON to the
// probe process, which runs as the agent.
type ProbeSpec struct {
	// ProxyURL is the URL of netguard.
	ProxyURL string `json:"proxy_url"`
	// DirectTargets are host:port addresses that must not answer a direct
	// TCP connection. An allowed host by name belongs here too: the agent
	// must reach it only through the proxy.
	DirectTargets []string `json:"direct_targets"`
	// DaemonPorts are loopback ports that the agent must not reach.
	DaemonPorts []int `json:"daemon_ports,omitempty"`
	// ControlHost is host:port of the control plane. A CONNECT through the
	// proxy must work (the positive control). Empty skips the check.
	ControlHost string `json:"control_host,omitempty"`
	// Restricted says that the proxy enforces a restricted policy, so a
	// CONNECT to ProbeDeniedHost must get 403.
	Restricted bool `json:"restricted"`
	// TimeoutMS is the time of one connection attempt. Default 2000.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// Check is the result of one probe check.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ProbeResult is the output of the probe.
type ProbeResult struct {
	Checks []Check `json:"checks"`
}

// Err returns nil when every check passed, else an error that names the
// failed checks. A result without checks is a failure: the probe did not run.
func (r ProbeResult) Err() error {
	if len(r.Checks) == 0 {
		return errors.New("netlock: the probe ran no check")
	}
	var bad []string
	for _, c := range r.Checks {
		if !c.OK {
			bad = append(bad, c.Name+" ("+c.Detail+")")
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("netlock: the lock is not proven: %s", strings.Join(bad, "; "))
	}
	return nil
}

// DialFunc opens a connection.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// RunProbe runs the checks of spec. dial nil means a net.Dialer with the
// attempt timeout.
func RunProbe(ctx context.Context, spec ProbeSpec, dial DialFunc) ProbeResult {
	timeout := time.Duration(spec.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if dial == nil {
		d := &net.Dialer{Timeout: timeout}
		dial = d.DialContext
	}
	attempt := func(addr string) error {
		actx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		c, err := dial(actx, "tcp", addr)
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	var res ProbeResult
	add := func(name string, ok bool, detail string) {
		res.Checks = append(res.Checks, Check{Name: name, OK: ok, Detail: detail})
	}

	// Negative checks: a direct connection must fail.
	for _, t := range spec.DirectTargets {
		if err := attempt(t); err == nil {
			add("direct "+t, false, "connected without the proxy")
		} else {
			add("direct "+t, true, "")
		}
	}
	for _, port := range spec.DaemonPorts {
		for _, host := range []string{"127.0.0.1", "::1"} {
			addr := net.JoinHostPort(host, strconv.Itoa(port))
			if err := attempt(addr); err == nil {
				add("daemon "+addr, false, "connected to a provider daemon")
			} else {
				add("daemon "+addr, true, "")
			}
		}
	}

	// The agent keeps its own loopback.
	ok, detail := loopbackWorks(ctx, dial, timeout)
	add("loopback", ok, detail)

	// Positive control through the proxy.
	if spec.ControlHost != "" {
		code, err := connectStatus(ctx, dial, spec.ProxyURL, spec.ControlHost, timeout)
		switch {
		case err != nil:
			add("proxy "+spec.ControlHost, false, err.Error())
		case code != http.StatusOK:
			add("proxy "+spec.ControlHost, false, "status "+strconv.Itoa(code))
		default:
			add("proxy "+spec.ControlHost, true, "")
		}
	}
	if spec.Restricted {
		code, err := connectStatus(ctx, dial, spec.ProxyURL, ProbeDeniedHost, timeout)
		switch {
		case err != nil:
			add("proxy denies", false, err.Error())
		case code != http.StatusForbidden:
			add("proxy denies", false, "status "+strconv.Itoa(code))
		default:
			add("proxy denies", true, "")
		}
	}
	return res
}

// loopbackWorks listens on 127.0.0.1 and connects to it.
func loopbackWorks(ctx context.Context, dial DialFunc, timeout time.Duration) (bool, string) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return false, "listen: " + err.Error()
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			_ = c.Close()
		}
	}()
	actx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c, err := dial(actx, "tcp", ln.Addr().String())
	if err != nil {
		return false, "connect: " + err.Error()
	}
	_ = c.Close()
	return true, ""
}

// connectStatus sends "CONNECT target" to the proxy and returns the status.
func connectStatus(ctx context.Context, dial DialFunc, proxyURL, target string, timeout time.Duration) (int, error) {
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return 0, fmt.Errorf("bad proxy URL %q", proxyURL)
	}
	actx, cancel := context.WithTimeout(ctx, timeout*3)
	defer cancel()
	c, err := dial(actx, "tcp", u.Host)
	if err != nil {
		return 0, fmt.Errorf("connect to the proxy: %w", err)
	}
	defer c.Close()
	if dl, ok := actx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	if _, err := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target); err != nil {
		return 0, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}
