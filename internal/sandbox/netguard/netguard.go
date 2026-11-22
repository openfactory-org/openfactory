// Package netguard is an HTTP forward proxy (plain HTTP and CONNECT tunnels)
// that enforces a rtcore.EffectiveNetworkPolicy for a sandbox.
//
// Policy semantics (matching the factory networkPolicy reference):
//   - Hosts required by the platform (alwaysAllow: repository forges, MCP
//     servers, image registries, the control plane) are always reachable and
//     cannot be denied.
//   - A denied host is blocked even when it is also allowed.
//   - When the policy is restricted, only allowed hosts are reachable.
//   - When the policy is unrestricted, everything not denied is reachable.
//
// Policy host entries are exact hostnames or literal IPs (case-insensitive,
// no ports, wildcards or CIDRs). alwaysAllow entries additionally accept a
// leading "." or "*." to match subdomains, since platform services are often
// spread across subdomains.
//
// Blocked attempts are recorded so runs can report blocked-host diagnostics
// (code "network/egress_blocked").
package netguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// DiagnosticCode is the diagnostic code reported for blocked egress.
const DiagnosticCode = "network/egress_blocked"

// BlockedHost aggregates blocked attempts for one host.
type BlockedHost struct {
	Host  string    `json:"host"`
	Count int       `json:"count"`
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
}

// Diagnostic is a run diagnostic entry describing a blocked host.
type Diagnostic struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Data      map[string]string `json:"data"`
	Timestamp time.Time         `json:"timestamp"`
}

// Proxy enforces a network policy. Create with New.
type Proxy struct {
	// OnBlocked, if set, is called (synchronously) for every blocked attempt.
	OnBlocked func(host string)
	// Logger receives blocked-host warnings (slog.Default when nil).
	Logger *slog.Logger
	// DialTimeout bounds upstream dials (default 30s).
	DialTimeout time.Duration

	mu         sync.RWMutex
	restricted bool
	allow      map[string]bool
	deny       map[string]bool
	always     []string
	// private lists the hosts that may resolve to a reserved address.
	private map[string]bool

	bmu     sync.Mutex
	blocked map[string]*BlockedHost

	transport *http.Transport

	smu sync.Mutex
	srv *http.Server
	ln  net.Listener
}

// New creates a proxy for policy. alwaysAllow lists platform hosts that are
// reachable regardless of the policy.
func New(policy rtcore.EffectiveNetworkPolicy, alwaysAllow []string) *Proxy {
	p := &Proxy{blocked: map[string]*BlockedHost{}}
	p.SetPolicy(policy, alwaysAllow)
	p.transport = &http.Transport{
		Proxy:                 nil,
		DialContext:           p.dialContext,
		ForceAttemptHTTP2:     false,
		MaxIdleConns:          64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
	return p
}

// SetPolicy replaces the enforced policy.
func (p *Proxy) SetPolicy(policy rtcore.EffectiveNetworkPolicy, alwaysAllow []string) {
	allow := map[string]bool{}
	for _, h := range policy.AllowedHosts {
		if n := NormalizeHost(h); n != "" {
			allow[n] = true
		}
	}
	deny := map[string]bool{}
	for _, h := range policy.DeniedHosts {
		if n := NormalizeHost(h); n != "" {
			deny[n] = true
		}
	}
	var always []string
	for _, h := range alwaysAllow {
		h = strings.TrimSpace(strings.ToLower(h))
		prefix := ""
		switch {
		case strings.HasPrefix(h, "*."):
			prefix, h = "*.", h[2:]
		case strings.HasPrefix(h, "."):
			prefix, h = ".", h[1:]
		}
		if n := NormalizeHost(h); n != "" {
			always = append(always, prefix+n)
		}
	}
	p.mu.Lock()
	p.restricted, p.allow, p.deny, p.always = policy.Restricted, allow, deny, always
	p.mu.Unlock()
}

// AllowPrivateHosts sets the hosts (exact names or IP literals) that may
// resolve to a reserved address: a private network, loopback, link-local and
// the other ranges of ReservedAddr. Every other host is refused at dial time
// when its address is reserved, even when the policy allows its name. Use it
// for platform hosts on a private network, for example a control plane on
// localhost during development or a self-managed forge.
func (p *Proxy) AllowPrivateHosts(hosts []string) {
	m := map[string]bool{}
	for _, h := range hosts {
		if n := NormalizeHost(h); n != "" {
			m[n] = true
		}
	}
	p.mu.Lock()
	p.private = m
	p.mu.Unlock()
}

func (p *Proxy) privateAllowed(host string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.private[NormalizeHost(host)]
}

// NormalizeHost lowercases a host, strips ports, IPv6 brackets and trailing dots.
func NormalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.TrimPrefix(strings.TrimSuffix(h, "]"), "[")
	h = strings.TrimSuffix(h, ".")
	return h
}

// Allowed reports whether host (optionally with port) may be reached.
func (p *Proxy) Allowed(host string) bool {
	h := NormalizeHost(host)
	if h == "" {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, a := range p.always {
		switch {
		case strings.HasPrefix(a, "*."):
			if strings.HasSuffix(h, a[1:]) {
				return true
			}
		case strings.HasPrefix(a, "."):
			if h == a[1:] || strings.HasSuffix(h, a) {
				return true
			}
		default:
			if h == a {
				return true
			}
		}
	}
	if p.deny[h] {
		return false
	}
	if !p.restricted {
		return true
	}
	return p.allow[h]
}

func (p *Proxy) logger() *slog.Logger {
	if p.Logger != nil {
		return p.Logger
	}
	return slog.Default()
}

// recordBlocked records a blocked attempt.
func (p *Proxy) recordBlocked(host string) {
	h := NormalizeHost(host)
	now := time.Now().UTC()
	p.bmu.Lock()
	b, ok := p.blocked[h]
	if !ok {
		b = &BlockedHost{Host: h, First: now}
		p.blocked[h] = b
	}
	b.Count++
	b.Last = now
	p.bmu.Unlock()
	if !ok {
		p.logger().Warn("network policy blocked egress", "host", h)
	}
	if p.OnBlocked != nil {
		p.OnBlocked(h)
	}
}

// Blocked returns blocked hosts sorted by first occurrence.
func (p *Proxy) Blocked() []BlockedHost {
	p.bmu.Lock()
	defer p.bmu.Unlock()
	out := make([]BlockedHost, 0, len(p.blocked))
	for _, b := range p.blocked {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].First.Equal(out[j].First) {
			return out[i].Host < out[j].Host
		}
		return out[i].First.Before(out[j].First)
	})
	return out
}

// Diagnostics renders blocked hosts as run diagnostics.
func (p *Proxy) Diagnostics() []Diagnostic {
	var out []Diagnostic
	for _, b := range p.Blocked() {
		out = append(out, Diagnostic{
			Code:      DiagnosticCode,
			Message:   fmt.Sprintf("Network access to %s blocked", b.Host),
			Data:      map[string]string{"blocked_host": b.Host},
			Timestamp: b.First,
		})
	}
	return out
}

func (p *Proxy) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	// Re-check at dial time so redirects or keep-alive reuse cannot bypass policy.
	if !p.Allowed(host) {
		p.recordBlocked(host)
		return nil, &blockedError{host: host}
	}
	timeout := p.DialTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	d := net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	if !p.privateAllowed(host) {
		// Check the address that is really dialed, after name resolution.
		d.Control = dialControl(NormalizeHost(host))
	}
	conn, err := d.DialContext(ctx, network, addr)
	if err != nil {
		if re, ok := isReservedAddrError(err); ok {
			p.recordBlocked(host)
			return nil, &blockedError{host: host, reason: re.Error()}
		}
		return nil, err
	}
	return conn, nil
}

type blockedError struct {
	host   string
	reason string
}

func (e *blockedError) Error() string {
	if e.reason != "" {
		return e.reason
	}
	return "egress to " + e.host + " blocked by network policy"
}

func blockedMessage(host string) string {
	return fmt.Sprintf("Open Factory network policy blocked access to %s\n", NormalizeHost(host))
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// ServeHTTP implements the proxy.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "netguard: this is a forward proxy; absolute request URI required", http.StatusBadRequest)
		return
	}
	if r.URL.Scheme != "http" {
		http.Error(w, "netguard: unsupported scheme "+r.URL.Scheme, http.StatusBadRequest)
		return
	}
	if !p.Allowed(r.URL.Hostname()) {
		p.recordBlocked(r.URL.Hostname())
		http.Error(w, blockedMessage(r.URL.Hostname()), http.StatusForbidden)
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	removeHopHeaders(out.Header)
	if c := r.Header.Get("Connection"); c != "" {
		for _, f := range strings.Split(c, ",") {
			out.Header.Del(strings.TrimSpace(f))
		}
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		var be *blockedError
		if errors.As(err, &be) {
			http.Error(w, blockedMessage(be.host), http.StatusForbidden)
			return
		}
		http.Error(w, "netguard: upstream error: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(flushWriter{w}, resp.Body)
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

func removeHopHeaders(h http.Header) {
	for _, k := range hopHeaders {
		h.Del(k)
	}
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	host, port, err := net.SplitHostPort(target)
	if err != nil || port == "" {
		http.Error(w, "netguard: CONNECT target must be host:port", http.StatusBadRequest)
		return
	}
	if !p.Allowed(host) {
		p.recordBlocked(host)
		http.Error(w, blockedMessage(host), http.StatusForbidden)
		return
	}
	upstream, err := p.dialContext(r.Context(), "tcp", target)
	if err != nil {
		var be *blockedError
		if errors.As(err, &be) {
			http.Error(w, blockedMessage(be.host), http.StatusForbidden)
			return
		}
		http.Error(w, "netguard: dial "+target+": "+err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		upstream.Close()
		http.Error(w, "netguard: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	client, brw, err := hj.Hijack()
	if err != nil {
		upstream.Close()
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		client.Close()
		upstream.Close()
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// Forward any bytes the client sent after the CONNECT headers.
		if n := brw.Reader.Buffered(); n > 0 {
			buf, _ := brw.Peek(n)
			_, _ = upstream.Write(buf)
		}
		_, _ = io.Copy(upstream, client)
		closeWrite(upstream)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
	}()
	wg.Wait()
	client.Close()
	upstream.Close()
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// Serve serves the proxy on l until Close.
func (p *Proxy) Serve(l net.Listener) error {
	srv := &http.Server{Handler: p, ReadHeaderTimeout: 30 * time.Second}
	p.smu.Lock()
	p.srv, p.ln = srv, l
	p.smu.Unlock()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ListenAndServe listens on addr and serves until Close.
func (p *Proxy) ListenAndServe(addr string) error {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	return p.Serve(l)
}

// Start listens on addr (e.g. "127.0.0.1:0") and serves in the background.
// It returns the bound address.
func (p *Proxy) Start(addr string) (net.Addr, error) {
	l, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		return nil, err
	}
	go func() {
		if err := p.Serve(l); err != nil {
			p.logger().Error("netguard proxy stopped", "err", err)
		}
	}()
	return l.Addr(), nil
}

// Addr returns the listening address, or nil before Serve.
func (p *Proxy) Addr() net.Addr {
	p.smu.Lock()
	defer p.smu.Unlock()
	if p.ln == nil {
		return nil
	}
	return p.ln.Addr()
}

// Close stops the proxy and closes idle upstream connections.
func (p *Proxy) Close() error {
	p.smu.Lock()
	srv := p.srv
	p.smu.Unlock()
	p.transport.CloseIdleConnections()
	if srv == nil {
		return nil
	}
	return srv.Close()
}

// ReportPath is the path of the report that ReportHandler serves.
const ReportPath = "/blocked"

// Report is the body of the blocked-host report. The runner of a task
// reads it from the sidecar and sends one diagnostic for each host.
type Report struct {
	Blocked []BlockedHost `json:"blocked"`
}

// ReportHandler serves the hosts that the proxy blocked, as JSON at
// ReportPath. It has no secret. Serve it on a loopback address only: the task
// container and the sidecar share the network of the Pod.
func (p *Proxy) ReportHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+ReportPath, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Report{Blocked: p.Blocked()})
	})
	return mux
}
