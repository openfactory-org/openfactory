package netguard

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

// TestStartThenCloseAtOnce checks that Addr and Close see the server as soon
// as Start returns.
func TestStartThenCloseAtOnce(t *testing.T) {
	for i := 0; i < 50; i++ {
		p := New(rtcore.EffectiveNetworkPolicy{}, nil)
		addr, err := p.Start("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		if p.Addr() == nil {
			t.Fatal("Addr is nil right after Start")
		}
		if err := p.Close(); err != nil {
			t.Fatal(err)
		}
		c, err := net.DialTimeout("tcp", addr.String(), time.Second)
		if err == nil {
			_ = c.Close()
			t.Fatalf("round %d: the proxy still listens after Close", i)
		}
	}
}

func TestServeAfterClose(t *testing.T) {
	p := New(rtcore.EffectiveNetworkPolicy{}, nil)
	_ = p.Close()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Serve(l); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
		_ = c.Close()
		t.Fatal("Serve after Close must close the listener")
	}
	if _, err := p.Start("127.0.0.1:0"); err == nil {
		t.Fatal("Start after Close must fail")
	}
}

// TestBlockedRecordIsBounded blocks more distinct hosts than the record keeps.
func TestBlockedRecordIsBounded(t *testing.T) {
	p := New(rtcore.EffectiveNetworkPolicy{Restricted: true}, nil)
	p.Logger = slog.New(slog.DiscardHandler)
	var calls int
	p.OnBlocked = func(string) { calls++ }
	for i := 0; i < maxBlockedHosts+100; i++ {
		p.recordBlocked(fmt.Sprintf("h%d.example", i))
	}
	p.recordBlocked(strings.Repeat("a", 10000) + ".example")
	p.recordBlocked("h0.example")
	got := p.Blocked()
	if len(got) != maxBlockedHosts {
		t.Fatalf("record has %d hosts, want %d", len(got), maxBlockedHosts)
	}
	for _, b := range got {
		if len(b.Host) > maxHostLen {
			t.Fatalf("host of %d bytes kept", len(b.Host))
		}
		if b.Host == "h0.example" && b.Count != 2 {
			t.Fatalf("known host count %d, want 2", b.Count)
		}
	}
	if calls != maxBlockedHosts+102 {
		t.Fatalf("OnBlocked called %d times, want every attempt", calls)
	}
}
