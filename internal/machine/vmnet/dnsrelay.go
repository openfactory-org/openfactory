package vmnet

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// DNSRelay forwards raw DNS packets from one VM to the host resolvers. It
// does not parse DNS content. An unrestricted VM uses it; a restricted VM
// has no DNS.
type DNSRelay struct {
	Listen    string
	Upstreams []netip.Addr
	Logger    *slog.Logger
	// Timeout of one upstream exchange (5 s).
	Timeout time.Duration
	// MaxInflight bounds the UDP exchanges and the TCP connections that run
	// at the same time (256). The relay drops a UDP query and closes a TCP
	// connection above the bound: the guest resolver retries.
	MaxInflight int
	// port of the upstreams; 0 means 53. Tests change it.
	port int
}

// Run serves UDP and TCP on Listen until ctx ends.
func (r *DNSRelay) Run(ctx context.Context) error {
	if len(r.Upstreams) == 0 {
		return errors.New("dns relay: no upstream resolver")
	}
	if r.Timeout <= 0 {
		r.Timeout = 5 * time.Second
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if r.MaxInflight <= 0 {
		r.MaxInflight = 256
	}
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(ctx, "udp4", r.Listen)
	if err != nil {
		return err
	}
	ln, err := lc.Listen(ctx, "tcp4", r.Listen)
	if err != nil {
		_ = pc.Close()
		return err
	}
	go func() {
		<-ctx.Done()
		_ = pc.Close()
		_ = ln.Close()
	}()
	sem := make(chan struct{}, r.MaxInflight)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); r.serveUDP(pc, sem) }()
	go func() { defer wg.Done(); r.serveTCP(ln, sem) }()
	wg.Wait()
	return ctx.Err()
}

func (r *DNSRelay) upstream(i int) string {
	port := r.port
	if port == 0 {
		port = DNSPort
	}
	return net.JoinHostPort(r.Upstreams[i%len(r.Upstreams)].String(), strconv.Itoa(port))
}

// udpBufs holds the 64 KiB buffers of the UDP exchanges.
var udpBufs = sync.Pool{New: func() any { b := make([]byte, 65535); return &b }}

// acquire takes a slot of sem without waiting.
func acquire(sem chan struct{}) bool {
	select {
	case sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (r *DNSRelay) serveUDP(pc net.PacketConn, sem chan struct{}) {
	buf := make([]byte, 65535)
	n := 0
	for {
		size, from, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		if !acquire(sem) {
			continue // too many exchanges: drop the query
		}
		bp := udpBufs.Get().(*[]byte)
		q := append((*bp)[:0], buf[:size]...)
		up := r.upstream(n)
		n++
		go func() {
			defer func() { udpBufs.Put(bp); <-sem }()
			d := net.Dialer{Timeout: r.Timeout}
			c, err := d.DialContext(context.Background(), "udp4", up)
			if err != nil {
				return
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(r.Timeout))
			if _, err := c.Write(q); err != nil {
				return
			}
			// The query is sent: read the answer into the same buffer.
			resp := (*bp)[:cap(*bp)]
			m, err := c.Read(resp)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(resp[:m], from)
		}()
	}
}

func (r *DNSRelay) serveTCP(ln net.Listener, sem chan struct{}) {
	n := 0
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !acquire(sem) {
			_ = c.Close() // too many connections
			continue
		}
		up := r.upstream(n)
		n++
		go func() {
			defer func() { <-sem }()
			defer c.Close()
			d := net.Dialer{Timeout: r.Timeout}
			u, err := d.DialContext(context.Background(), "tcp4", up)
			if err != nil {
				return
			}
			defer u.Close()
			deadline := time.Now().Add(30 * time.Second)
			_ = c.SetDeadline(deadline)
			_ = u.SetDeadline(deadline)
			go func() { _, _ = io.Copy(u, c) }()
			_, _ = io.Copy(c, u)
		}()
	}
}
