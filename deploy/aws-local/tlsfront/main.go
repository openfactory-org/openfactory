// Command tlsfront is a test-only TLS front for a local AWS emulator. NOT FOR
// PRODUCTION.
//
// The emulator serves HTTPS with a certificate for localhost and 127.0.0.1
// only, and makes a new one at each start. Sandbox Pods, the server and a
// browser must reach it under one host name, because SigV4 signs the host of
// presigned URLs. tlsfront listens with a certificate for that name and
// forwards each request to the emulator on 127.0.0.1, keeping the Host header,
// so signatures made for the public name still match.
//
// The path prefix /__pod-identity/ is not an AWS request: tlsfront sends it
// to the Pod Identity broker of the Pod (deploy/aws-local/podidentity), with
// the prefix removed.
package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func main() {
	listen := flag.String("listen", ":9443", "address to listen on")
	cert := flag.String("cert", "/etc/tlsfront/tls.crt", "server certificate (PEM)")
	key := flag.String("key", "/etc/tlsfront/tls.key", "server key (PEM)")
	upstream := flag.String("upstream", "https://127.0.0.1:9297", "emulator URL")
	upstreamCA := flag.String("upstream-ca", "", "CA file of the emulator; tlsfront waits until it exists")
	wait := flag.Duration("wait", 2*time.Minute, "how long to wait for -upstream-ca")
	stsHost := flag.String("unsigned-sts-host", "sts.amazonaws.com", "Host header for unsigned form POSTs to /")
	s3Host := flag.String("unsigned-s3-host", "s3.amazonaws.com", "Host header for other unsigned requests")
	podIdentity := flag.String("pod-identity-upstream", "http://127.0.0.1:2704", "URL of the Pod Identity broker for /__pod-identity/")
	flag.Parse()

	target, err := url.Parse(*upstream)
	if err != nil {
		log.Fatalf("tlsfront: bad -upstream: %v", err)
	}
	broker, err := url.Parse(*podIdentity)
	if err != nil {
		log.Fatalf("tlsfront: bad -pod-identity-upstream: %v", err)
	}
	pool := x509.NewCertPool()
	deadline := time.Now().Add(*wait)
	for {
		pem, err := os.ReadFile(*upstreamCA)
		if err == nil && pool.AppendCertsFromPEM(pem) {
			break
		}
		if time.Now().After(deadline) {
			log.Fatalf("tlsfront: no CA in %s after %s", *upstreamCA, *wait)
		}
		time.Sleep(500 * time.Millisecond)
	}

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	// The emulator certificate names localhost; the Host header carries the public name.
	tr.TLSClientConfig = &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Host = unsignedHost(r.In, *stsHost, *s3Host)
		},
		Transport:     tr,
		FlushInterval: -1,
	}
	srv := &http.Server{Addr: *listen, Handler: route(proxy, newBrokerProxy(broker)), ReadHeaderTimeout: 30 * time.Second}
	log.Printf("tlsfront: %s -> %s, %s -> %s", *listen, *upstream, podIdentityPrefix, *podIdentity)
	log.Fatal(srv.ListenAndServeTLS(*cert, *key))
}

// podIdentityPrefix is the path prefix of the Pod Identity broker.
const podIdentityPrefix = "/__pod-identity/"

// route sends the broker's prefix to broker and everything else to emulator.
func route(emulator, broker http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, podIdentityPrefix) {
			broker.ServeHTTP(w, r)
			return
		}
		emulator.ServeHTTP(w, r)
	})
}

// newBrokerProxy forwards to the broker at target with the prefix removed.
// The request is not an AWS request, so the Host header is the broker's own.
func newBrokerProxy(target *url.URL) http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.URL.Path = stripPodIdentity(r.In.URL.Path)
			r.Out.URL.RawPath = ""
			r.Out.Host = target.Host
		},
		Transport: &http.Transport{Proxy: nil}, // loopback: never through a proxy
	}
}

// stripPodIdentity removes the broker prefix and keeps the leading slash.
func stripPodIdentity(p string) string {
	return "/" + strings.TrimPrefix(p, podIdentityPrefix)
}

// unsignedHost returns the Host header to send to the emulator: the
// request's own for a signed request, an admin route or the broker route, else an AWS host name
// that selects the service.
func unsignedHost(r *http.Request, stsHost, s3Host string) string {
	q := r.URL.Query()
	signed := r.Header.Get("Authorization") != "" || q.Get("X-Amz-Signature") != "" || q.Get("X-Amz-Credential") != ""
	if signed || strings.HasPrefix(r.URL.Path, "/__admin/") || strings.HasPrefix(r.URL.Path, podIdentityPrefix) {
		return r.Host
	}
	if r.Method == http.MethodPost && (r.URL.Path == "/" || r.URL.Path == "") &&
		strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		return stsHost
	}
	return s3Host
}
