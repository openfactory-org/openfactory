package ws

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func echoServer(t *testing.T, protos []string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, protos)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			op, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if string(data) == "close" {
				_ = c.CloseWithStatus(CloseGoingAway, "bye")
				return
			}
			if err := c.WriteMessage(op, data); err != nil {
				return
			}
		}
	}))
}

func TestEchoAndSubprotocol(t *testing.T) {
	srv := echoServer(t, []string{"v5.channel.k8s.io", "v4.channel.k8s.io"})
	defer srv.Close()
	c, _, err := Dial(context.Background(), nil, "ws"+strings.TrimPrefix(srv.URL, "http"), nil, []string{"v9", "v4.channel.k8s.io"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Subprotocol != "v4.channel.k8s.io" {
		t.Fatalf("subprotocol %q", c.Subprotocol)
	}
	for _, size := range []int{0, 1, 125, 126, 65535, 65536, 1 << 20} {
		msg := bytes.Repeat([]byte{byte(size)}, size)
		if err := c.WriteMessage(OpBinary, msg); err != nil {
			t.Fatal(err)
		}
		op, got, err := c.ReadMessage()
		if err != nil || op != OpBinary || !bytes.Equal(got, msg) {
			t.Fatalf("size %d: op=%d len=%d err=%v", size, op, len(got), err)
		}
	}
	if err := c.WriteMessage(OpText, []byte("close")); err != nil {
		t.Fatal(err)
	}
	_, _, err = c.ReadMessage()
	var ce *CloseError
	if !errors.As(err, &ce) || ce.Code != CloseGoingAway || ce.Reason != "bye" || !IsNormalClose(err) {
		t.Fatalf("close err %v", err)
	}
}

func TestFragmentedAndPing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, nil)
		if err != nil {
			return
		}
		// Send a ping, then a message in three fragments with a ping in between.
		c.wmu.Lock()
		raw := func(fin bool, op int, data []byte) {
			b0 := byte(op)
			if fin {
				b0 |= 0x80
			}
			_, _ = c.rwc.Write(append([]byte{b0, byte(len(data))}, data...))
		}
		raw(true, OpPing, []byte("p"))
		raw(false, OpText, []byte("hel"))
		raw(true, OpPing, nil)
		raw(false, OpContinuation, []byte("lo "))
		raw(true, OpContinuation, []byte("world"))
		c.wmu.Unlock()
		// Expect pongs and then an echo request.
		_, data, err := c.ReadMessage()
		if err == nil {
			_ = c.WriteMessage(OpText, data)
		}
		_ = c.Close()
	}))
	defer srv.Close()
	c, _, err := Dial(context.Background(), srv.Client(), srv.URL, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	op, data, err := c.ReadMessage()
	if err != nil || op != OpText || string(data) != "hello world" {
		t.Fatalf("fragmented: %d %q %v", op, data, err)
	}
	_ = c.WriteMessage(OpText, []byte("ack"))
	if _, data, err := c.ReadMessage(); err != nil || string(data) != "ack" {
		t.Fatalf("ack %q %v", data, err)
	}
}

func TestHandshakeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer srv.Close()
	_, _, err := Dial(context.Background(), nil, srv.URL, nil, nil)
	var he *HandshakeError
	if !errors.As(err, &he) || he.StatusCode != http.StatusUnauthorized {
		t.Fatalf("err %v", err)
	}
	// Plain GET to an upgrade endpoint fails cleanly.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = Upgrade(w, r, nil)
	}))
	defer up.Close()
	resp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestTLSDial(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = c.WriteMessage(OpText, []byte("tls ok"))
		_ = c.Close()
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	c, _, err := Dial(context.Background(), srv.Client(), "wss"+strings.TrimPrefix(srv.URL, "https"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, data, err := c.ReadMessage(); err != nil || string(data) != "tls ok" {
		t.Fatalf("%q %v", data, err)
	}
}
