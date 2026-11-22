package ws

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// plainRWC hides the deadline methods of a net.Conn.
type plainRWC struct{ c net.Conn }

func (p plainRWC) Read(b []byte) (int, error)  { return p.c.Read(b) }
func (p plainRWC) Write(b []byte) (int, error) { return p.c.Write(b) }
func (p plainRWC) Close() error                { return p.c.Close() }

// TestCloseDoesNotHangOnStuckPeer closes a connection whose peer never
// reads. The close frame cannot be written; Close must return anyway.
func TestCloseDoesNotHangOnStuckPeer(t *testing.T) {
	for name, wrap := range map[string]func(net.Conn) io.ReadWriteCloser{
		"with deadlines":    func(c net.Conn) io.ReadWriteCloser { return c },
		"without deadlines": func(c net.Conn) io.ReadWriteCloser { return plainRWC{c} },
	} {
		t.Run(name, func(t *testing.T) {
			a, b := net.Pipe() // writes block until the peer reads
			defer b.Close()
			c := newConn(wrap(a), nil, false, "")
			c.closeTimeout = 100 * time.Millisecond
			// A writer that blocks first and holds the write lock.
			go func() { _ = c.WriteMessage(OpText, []byte("stuck")) }()
			time.Sleep(20 * time.Millisecond)
			done := make(chan struct{})
			go func() { _ = c.Close(); close(done) }()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("Close hangs on a peer that does not read")
			}
			select {
			case <-c.Done():
			default:
				t.Fatal("Close did not shut the connection down")
			}
		})
	}
}

// TestRefusedDialReleasesConnection checks that a refused handshake does not
// leave the connection idle in the private transport of Dial.
func TestRefusedDialReleasesConnection(t *testing.T) {
	var closed atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateClosed {
			closed.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	_, _, err := Dial(context.Background(), &http.Client{Transport: &http.Transport{}}, srv.URL, nil, nil)
	var he *HandshakeError
	if !errors.As(err, &he) {
		t.Fatalf("err %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for closed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if closed.Load() == 0 {
		t.Fatal("the connection of a refused handshake stays open")
	}
}

// TestReadDeadline ends a read that waits for a silent peer.
func TestReadDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	c := newConn(plainRWC{a}, nil, true, "")
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	errc := make(chan error, 1)
	go func() { _, _, err := c.ReadMessage(); errc <- err }()
	select {
	case err := <-errc:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err %v, want os.ErrDeadlineExceeded", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the read deadline did not end the read")
	}

	// A cleared deadline does not close the connection.
	a2, b2 := net.Pipe()
	defer b2.Close()
	c2 := newConn(a2, nil, true, "")
	_ = c2.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	_ = c2.SetReadDeadline(time.Time{})
	time.Sleep(150 * time.Millisecond)
	select {
	case <-c2.Done():
		t.Fatal("a cleared deadline closed the connection")
	default:
	}
	_ = a2.Close()
}
