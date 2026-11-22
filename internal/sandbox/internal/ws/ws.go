// Package ws is a small, dependency-free RFC 6455 WebSocket implementation used by
// the direct worker protocol.
//
// It supports client dialing through a regular *http.Client (so TLS client
// certificates, custom CAs and proxies configured on the transport keep working),
// server-side upgrades through http.Hijacker, fragmented messages, ping/pong and
// the close handshake. It intentionally omits extensions (no permessage-deflate).
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 requires SHA-1 for the Sec-WebSocket-Accept key
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Opcodes.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// Close status codes.
const (
	CloseNormal          = 1000
	CloseGoingAway       = 1001
	CloseProtocolError   = 1002
	CloseMessageTooBig   = 1009
	CloseInternalError   = 1011
	closeNoStatusPresent = 1005
)

// DefaultMaxMessageSize bounds a single reassembled message.
const DefaultMaxMessageSize = 64 << 20

const acceptGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// defaultCloseTimeout bounds the write of the close frame. A peer that
// stopped reading (a half-open TCP connection) cannot hold Close longer.
const defaultCloseTimeout = 5 * time.Second

// CloseError is returned by ReadMessage when the peer closed the connection.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket closed: %d %s", e.Code, e.Reason)
}

// IsNormalClose reports whether err represents an orderly shutdown.
func IsNormalClose(err error) bool {
	var ce *CloseError
	if errors.As(err, &ce) {
		return ce.Code == CloseNormal || ce.Code == CloseGoingAway || ce.Code == closeNoStatusPresent
	}
	return errors.Is(err, io.EOF)
}

// Conn is a WebSocket connection. ReadMessage must be called from a single
// goroutine; WriteMessage is safe for concurrent use.
type Conn struct {
	rwc    io.ReadWriteCloser
	br     *bufio.Reader
	client bool

	// Subprotocol is the negotiated subprotocol ("" if none).
	Subprotocol string
	// MaxMessageSize bounds reassembled messages (DefaultMaxMessageSize when 0).
	MaxMessageSize int64

	wmu       sync.Mutex
	closeOnce sync.Once
	closeSent bool
	closed    chan struct{}
	// closeTimeout is defaultCloseTimeout; tests shorten it.
	closeTimeout time.Duration

	// dmu guards readTimer. readExpired is set when the read deadline
	// passed and closed the connection.
	dmu         sync.Mutex
	readTimer   *time.Timer
	readExpired atomic.Bool
}

func newConn(rwc io.ReadWriteCloser, br *bufio.Reader, client bool, proto string) *Conn {
	if br == nil {
		br = bufio.NewReader(rwc)
	}
	return &Conn{rwc: rwc, br: br, client: client, Subprotocol: proto, closed: make(chan struct{}), closeTimeout: defaultCloseTimeout}
}

// Dial opens a client connection. url may use ws, wss, http or https schemes.
// client may be nil (http.DefaultClient's transport is used). The transport is
// cloned and forced to HTTP/1.1 because WebSocket upgrades need it.
func Dial(ctx context.Context, client *http.Client, url string, header http.Header, subprotocols []string) (*Conn, *http.Response, error) {
	switch {
	case strings.HasPrefix(url, "ws://"):
		url = "http://" + strings.TrimPrefix(url, "ws://")
	case strings.HasPrefix(url, "wss://"):
		url = "https://" + strings.TrimPrefix(url, "wss://")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, err
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	var keyBytes [16]byte
	if _, err := rand.Read(keyBytes[:]); err != nil {
		return nil, nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes[:])
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", key)
	if len(subprotocols) > 0 {
		req.Header.Set("Sec-WebSocket-Protocol", strings.Join(subprotocols, ", "))
	}

	hc, own := http1Client(client)
	if own {
		// The transport is a private clone. A connection that is not
		// upgraded (a refused handshake) would stay in its idle pool forever.
		defer hc.CloseIdleConnections()
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		return nil, resp, &HandshakeError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	if !headerContainsToken(resp.Header, "Upgrade", "websocket") {
		resp.Body.Close()
		return nil, resp, errors.New("ws: server did not upgrade to websocket")
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		resp.Body.Close()
		return nil, resp, errors.New("ws: invalid Sec-WebSocket-Accept")
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, resp, errors.New("ws: response body is not writable")
	}
	return newConn(rwc, nil, true, resp.Header.Get("Sec-WebSocket-Protocol")), resp, nil
}

// HandshakeError is returned by Dial when the server refuses the upgrade.
type HandshakeError struct {
	StatusCode int
	Body       string
}

func (e *HandshakeError) Error() string {
	return fmt.Sprintf("ws: handshake failed: HTTP %d: %s", e.StatusCode, strings.TrimSpace(e.Body))
}

// http1Client returns a client forced to HTTP/1.1. own reports that its
// transport is a private clone that the caller must release.
func http1Client(c *http.Client) (_ *http.Client, own bool) {
	if c == nil {
		c = http.DefaultClient
	}
	var base *http.Transport
	switch t := c.Transport.(type) {
	case nil:
		base = http.DefaultTransport.(*http.Transport)
	case *http.Transport:
		base = t
	default:
		// Unknown round tripper (e.g. test wrapper); use as-is.
		cp := *c
		return &cp, false
	}
	t := base.Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if t.TLSClientConfig != nil {
		t.TLSClientConfig = t.TLSClientConfig.Clone()
	} else {
		t.TLSClientConfig = &tls.Config{}
	}
	t.TLSClientConfig.NextProtos = []string{"http/1.1"}
	cp := *c
	cp.Transport = t
	return &cp, true
}

// Upgrade performs the server side of the handshake. If subprotocols is
// non-empty, the first client-offered protocol in that list is selected.
func Upgrade(w http.ResponseWriter, r *http.Request, subprotocols []string) (*Conn, error) {
	if r.Method != http.MethodGet {
		http.Error(w, "websocket: method not allowed", http.StatusMethodNotAllowed)
		return nil, errors.New("ws: method not GET")
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		http.Error(w, "websocket: upgrade required", http.StatusUpgradeRequired)
		return nil, errors.New("ws: not a websocket upgrade")
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "websocket: unsupported version", http.StatusBadRequest)
		return nil, errors.New("ws: unsupported version")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "websocket: missing key", http.StatusBadRequest)
		return nil, errors.New("ws: missing key")
	}
	proto := ""
	if len(subprotocols) > 0 {
		offered := splitTokens(r.Header.Values("Sec-WebSocket-Protocol"))
	outer:
		for _, o := range offered {
			for _, s := range subprotocols {
				if o == s {
					proto = s
					break outer
				}
			}
		}
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket: hijacking unsupported", http.StatusInternalServerError)
		return nil, errors.New("ws: response writer cannot hijack")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + acceptKey(key) + "\r\n")
	if proto != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + proto + "\r\n")
	}
	b.WriteString("\r\n")
	_ = netConn.SetDeadline(time.Time{})
	if _, err := netConn.Write([]byte(b.String())); err != nil {
		netConn.Close()
		return nil, err
	}
	return newConn(netConn, brw.Reader, false, proto), nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + acceptGUID)) //nolint:gosec // RFC 6455 requires SHA-1 for the Sec-WebSocket-Accept key
	return base64.StdEncoding.EncodeToString(h[:])
}

func splitTokens(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func headerContainsToken(h http.Header, name, token string) bool {
	for _, t := range splitTokens(h.Values(name)) {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}

// WriteMessage writes a single unfragmented message.
func (c *Conn) WriteMessage(op int, data []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closeSent {
		return net.ErrClosed
	}
	return c.writeFrameLocked(op, data)
}

func (c *Conn) writeFrameLocked(op int, data []byte) error {
	var hdr [14]byte
	hdr[0] = 0x80 | byte(op) //nolint:gosec // op is one of the 4-bit opcode constants
	n := 2
	l := len(data)
	switch {
	case l < 126:
		hdr[1] = byte(l)
	case l <= 0xFFFF:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:], uint16(l))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:], uint64(l))
		n = 10
	}
	payload := data
	if c.client {
		hdr[1] |= 0x80
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		copy(hdr[n:], mask[:])
		n += 4
		payload = make([]byte, l)
		for i := range data {
			payload[i] = data[i] ^ mask[i%4]
		}
	}
	buf := make([]byte, 0, n+l)
	buf = append(buf, hdr[:n]...)
	buf = append(buf, payload...)
	_, err := c.rwc.Write(buf)
	return err
}

// ReadMessage returns the next data message (text or binary). Control frames are
// handled transparently. A *CloseError is returned when the peer closes.
func (c *Conn) ReadMessage() (int, []byte, error) {
	max := c.MaxMessageSize
	if max <= 0 {
		max = DefaultMaxMessageSize
	}
	var (
		msgOp int
		msg   []byte
		inMsg bool
	)
	for {
		fin, op, payload, err := c.readFrame(max)
		if err != nil {
			if c.readExpired.Load() {
				return 0, nil, fmt.Errorf("ws: read: %w", os.ErrDeadlineExceeded)
			}
			return 0, nil, err
		}
		switch op {
		case OpPing:
			c.wmu.Lock()
			if !c.closeSent {
				_ = c.writeFrameLocked(OpPong, payload)
			}
			c.wmu.Unlock()
			continue
		case OpPong:
			continue
		case OpClose:
			code, reason := closeNoStatusPresent, ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload))
				reason = string(payload[2:])
			}
			c.wmu.Lock()
			if !c.closeSent {
				echo := payload
				if len(echo) > 2 {
					echo = echo[:2]
				}
				_ = c.writeFrameLocked(OpClose, echo)
				c.closeSent = true
			}
			c.wmu.Unlock()
			c.shutdown()
			return 0, nil, &CloseError{Code: code, Reason: reason}
		case OpText, OpBinary:
			if inMsg {
				return 0, nil, c.fail(CloseProtocolError, "unexpected data frame inside fragmented message")
			}
			msgOp = op
			msg = append(msg[:0], payload...)
			inMsg = !fin
		case OpContinuation:
			if !inMsg {
				return 0, nil, c.fail(CloseProtocolError, "unexpected continuation frame")
			}
			if int64(len(msg)+len(payload)) > max {
				return 0, nil, c.fail(CloseMessageTooBig, "message too big")
			}
			msg = append(msg, payload...)
			inMsg = !fin
		default:
			return 0, nil, c.fail(CloseProtocolError, fmt.Sprintf("unknown opcode %d", op))
		}
		if !inMsg {
			return msgOp, msg, nil
		}
	}
}

func (c *Conn) readFrame(max int64) (bool, int, []byte, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin := h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		return false, 0, nil, c.fail(CloseProtocolError, "reserved bits set")
	}
	op := int(h[0] & 0x0F)
	masked := h[1]&0x80 != 0
	length := int64(h[1] & 0x7F)
	switch length {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.br, b[:]); err != nil {
			return false, 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(b[:])) //nolint:gosec // a negative result is refused below (length < 0)
	}
	if op >= OpClose && (length > 125 || !fin) {
		return false, 0, nil, c.fail(CloseProtocolError, "invalid control frame")
	}
	if length < 0 || length > max {
		return false, 0, nil, c.fail(CloseMessageTooBig, "frame too big")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, op, payload, nil
}

func (c *Conn) fail(code int, reason string) error {
	_ = c.CloseWithStatus(code, reason)
	return &CloseError{Code: code, Reason: reason}
}

// SetReadDeadline closes the connection when no ReadMessage call returned
// before t. ReadMessage then returns an error that wraps
// os.ErrDeadlineExceeded. Call it again after each message to keep an idle
// timeout. A zero t removes the deadline.
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.dmu.Lock()
	defer c.dmu.Unlock()
	if c.readTimer != nil {
		c.readTimer.Stop()
		c.readTimer = nil
	}
	if t.IsZero() {
		return nil
	}
	c.readTimer = time.AfterFunc(time.Until(t), func() {
		c.readExpired.Store(true)
		c.shutdown()
	})
	return nil
}

// CloseWithStatus sends a close frame (once) and closes the underlying
// connection. It returns within closeTimeout also when the peer stopped
// reading or another writer is stuck (after 5 s).
func (c *Conn) CloseWithStatus(code int, reason string) error {
	// Closing the connection ends a write that blocks, ours or another one
	// that holds wmu.
	force := time.AfterFunc(c.closeTimeout, c.shutdown)
	defer force.Stop()
	if d, ok := c.rwc.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Now().Add(c.closeTimeout))
	}
	c.wmu.Lock()
	if !c.closeSent {
		payload := make([]byte, 2+len(reason))
		binary.BigEndian.PutUint16(payload, uint16(code)) //nolint:gosec // close codes are 1000 to 4999
		copy(payload[2:], reason)
		_ = c.writeFrameLocked(OpClose, payload)
		c.closeSent = true
	}
	c.wmu.Unlock()
	c.shutdown()
	return nil
}

// Close performs a normal close.
func (c *Conn) Close() error { return c.CloseWithStatus(CloseNormal, "") }

// Done is closed once the connection has been shut down locally.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) shutdown() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.rwc.Close()
		c.dmu.Lock()
		if c.readTimer != nil {
			c.readTimer.Stop()
		}
		c.dmu.Unlock()
	})
}
