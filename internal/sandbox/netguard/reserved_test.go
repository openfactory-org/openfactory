package netguard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

func TestReservedAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"10.1.2.3", true},
		{"172.16.0.1", true},
		{"172.31.255.255", true},
		{"172.32.0.1", false},
		{"192.168.1.1", true},
		{"127.0.0.1", true},
		{"127.255.0.9", true},
		{"169.254.169.254", true},
		{"169.254.10.1", true},
		{"168.63.129.16", true},
		{"168.63.129.17", false},
		{"100.64.0.1", true},
		{"100.127.255.254", true},
		{"100.128.0.1", false},
		{"0.0.0.0", true},
		{"0.1.2.3", true},
		{"198.18.0.1", true},
		{"198.19.255.255", true},
		{"198.20.0.1", false},
		{"224.0.0.1", true},
		{"239.255.255.250", true},
		{"240.0.0.1", true},
		{"255.255.255.255", true},
		{"1.1.1.1", false},
		{"8.8.8.8", false},
		{"140.82.112.3", false},
		// IPv6
		{"::1", true},
		{"::", true},
		{"fe80::1", true},
		{"fe80::1%eth0", true},
		{"fc00::1", true},
		{"fd00:ec2::254", true},
		{"ff02::1", true},
		{"::ffff:10.0.0.1", true},
		{"::ffff:127.0.0.1", true},
		{"::ffff:169.254.169.254", true},
		{"::ffff:1.1.1.1", false},
		{"64:ff9b::a00:1", true},    // NAT64 of 10.0.0.1
		{"64:ff9b::101:101", false}, // NAT64 of 1.1.1.1
		{"2002:a00:1::1", true},     // 6to4 of 10.0.0.1
		{"2002:101:101::1", false},  // 6to4 of 1.1.1.1
		{"2001:db8::1", true},
		{"2001:0:4136:e378::1", true}, // Teredo
		{"2606:4700:4700::1111", false},
	}
	for _, tt := range tests {
		a, err := netip.ParseAddr(tt.addr)
		if err != nil {
			t.Fatalf("parse %q: %v", tt.addr, err)
		}
		if got := ReservedAddr(a); got != tt.want {
			t.Errorf("ReservedAddr(%s) = %v, want %v", tt.addr, got, tt.want)
		}
	}
	if !ReservedAddr(netip.Addr{}) {
		t.Error("invalid address is not reserved")
	}
}

// TestDialRefusesReservedAddress checks the DNS rebinding case: the policy
// allows a host name, and the name resolves to a private address.
func TestDialRefusesReservedAddress(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	port := u.Port()

	// "localhost" resolves to a loopback address.
	p := New(rtcore.EffectiveNetworkPolicy{Restricted: true, AllowedHosts: []string{"localhost", "127.0.0.1"}}, []string{"always.localhost"})
	var blocked []string
	p.OnBlocked = func(h string) { blocked = append(blocked, h) }
	addr, err := p.Start("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	proxyURL, _ := url.Parse("http://" + addr.String())
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	for _, target := range []string{"http://localhost:" + port + "/", "http://127.0.0.1:" + port + "/"} {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s: status %d (%s), want 403", target, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "reached") {
			t.Errorf("GET %s reached the upstream", target)
		}
	}

	// CONNECT to a reserved address is refused too.
	req, _ := http.NewRequest(http.MethodConnect, "http://"+addr.String(), nil)
	req.Host = "127.0.0.1:" + port
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT to a loopback address: status %d, want 403", resp.StatusCode)
	}
	if len(blocked) < 3 {
		t.Errorf("blocked = %v, want 3 records", blocked)
	}

	// A host on the private allow list may resolve to a reserved address.
	p.AllowPrivateHosts([]string{"localhost"})
	resp, err = client.Get("http://localhost:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "reached" {
		t.Errorf("private allow list: status %d body %q", resp.StatusCode, body)
	}
	// 127.0.0.1 is still refused: the list matches exact hosts.
	resp, err = client.Get("http://127.0.0.1:" + port + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("127.0.0.1 not in the private list: status %d, want 403", resp.StatusCode)
	}
}
