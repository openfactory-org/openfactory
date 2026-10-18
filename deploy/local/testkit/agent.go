package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	agentTimeout    = 10 * time.Second
	maxAgentReplyMB = 1
)

// newAgentHandler makes the handler of the Pod Identity agent stand-in. It
// forwards GET /v1/credentials with the Authorization header unchanged to
// <upstream>/v1/credentials and returns the upstream status, content type and
// body. The upstream is HTTPS; caFile (optional) is added to the system roots.
func newAgentHandler(upstream, caFile string, logger *log.Logger) (http.Handler, error) {
	u, err := url.Parse(strings.TrimRight(upstream, "/"))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("-upstream must be an http(s) URL, got %q", upstream)
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if caFile != "" {
		pem, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("-ca: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("-ca %s: no certificate found", caFile)
		}
	}
	client := &http.Client{
		Timeout:   agentTimeout,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
	return agentHandler(u.String()+"/v1/credentials", client, logger), nil
}

func agentHandler(target string, client *http.Client, logger *log.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("/v1/credentials", func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		status := forwardCredentials(w, r, target, client)
		// The line has no token and no body.
		logger.Printf("GET /v1/credentials status=%d duration=%s", status, time.Since(start).Round(time.Millisecond))
	})
	return mux
}

func forwardCredentials(w http.ResponseWriter, r *http.Request, target string, client *http.Client) int {
	if r.Method != http.MethodGet {
		return agentError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		return agentError(w, http.StatusInternalServerError, "build request")
	}
	if v := r.Header.Get("Authorization"); v != "" {
		req.Header.Set("Authorization", v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return agentError(w, http.StatusBadGateway, "the credential broker is not reachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAgentReplyMB<<20))
	if err != nil {
		return agentError(w, http.StatusBadGateway, "read the reply of the credential broker")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return resp.StatusCode
}

func agentError(w http.ResponseWriter, status int, msg string) int {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"code": http.StatusText(status), "message": msg})
	return status
}
