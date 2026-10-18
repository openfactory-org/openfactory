// Command podidentity is a test-only EKS Pod Identity broker for a local AWS
// emulator. NOT FOR PRODUCTION.
//
// The emulated EKS cluster API listens on a random port on 127.0.0.1 inside
// the emulator Pod, so only that Pod can reach it. The broker runs in the
// Pod and answers GET /v1/credentials, the call that a Pod Identity agent
// makes for a Pod. Steps:
//
//  1. Check the projected service account token with a TokenReview in the
//     real cluster (audience pods.eks.amazonaws.com). This gives the
//     namespace and the service account.
//  2. Read the endpoint and the CA of the emulated cluster (DescribeCluster)
//     and make an EKS bearer token, as "aws eks get-token" does.
//  3. Make sure the namespace and the service account exist in the emulated
//     cluster, and request a token for the service account.
//  4. Call eks-auth:AssumeRoleForPodIdentity with that token. The emulator
//     finds the Pod Identity association and returns role credentials.
//
// The broker listens on loopback only. tlsfront forwards /__pod-identity/ to it.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	home, _ := os.UserHomeDir()
	listen := flag.String("listen", "127.0.0.1:2704", "address to listen on (loopback)")
	cluster := flag.String("cluster", env("CLUSTER_NAME", "of-local"), "name of the emulated EKS cluster (env CLUSTER_NAME)")
	capStr := flag.String("expiry-cap", env("POD_IDENTITY_EXPIRY_CAP", ""), "longest life of returned credentials, a Go duration; 0 or empty: no cap (env POD_IDENTITY_EXPIRY_CAP)")
	region := flag.String("region", env("AWS_REGION", "us-east-1"), "AWS Region of the emulator")
	emuURL := flag.String("emulator", "https://127.0.0.1:9297", "URL of the emulator")
	emuDir := flag.String("emulator-dir", filepath.Join(home, ".aws", "emulator", "9297"), "directory with ca.pem and emulator_credentials of this run")
	emuServerName := flag.String("emulator-server-name", "localhost", "TLS server name of the emulator certificate")
	stsHost := flag.String("sts-host", "", "host in the EKS bearer token; default sts.<region>.amazonaws.com")
	kubeURL := flag.String("kube-api", defaultKubeAPI(), "URL of the Kubernetes API for TokenReview")
	kubeCA := flag.String("kube-ca", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", "CA file of the Kubernetes API")
	kubeToken := flag.String("kube-token", "/var/run/secrets/kubernetes.io/serviceaccount/token", "service account token file of the broker")
	wait := flag.Duration("wait", 2*time.Minute, "how long to wait for the emulator files at start")
	flag.Parse()

	var expiryCap time.Duration
	if *capStr != "" {
		d, err := time.ParseDuration(*capStr)
		if err != nil || d < 0 {
			log.Fatalf("podidentity: bad expiry cap %q", *capStr)
		}
		expiryCap = d
	}
	if *stsHost == "" {
		*stsHost = "sts." + *region + ".amazonaws.com"
	}

	emuCAFile := filepath.Join(*emuDir, "ca.pem")
	if err := waitFor(*wait, emuCAFile, filepath.Join(*emuDir, "emulator_credentials")); err != nil {
		log.Fatalf("podidentity: %v", err)
	}
	emuPool, err := poolFromFile(emuCAFile)
	if err != nil {
		log.Fatalf("podidentity: %v", err)
	}
	kubePool, err := poolFromFile(*kubeCA)
	if err != nil {
		log.Fatalf("podidentity: %v", err)
	}
	b := &broker{
		cluster:   *cluster,
		region:    *region,
		stsHost:   *stsHost,
		expiryCap: expiryCap,
		emuURL:    *emuURL,
		emuHTTP:   httpClient(&tls.Config{RootCAs: emuPool, ServerName: *emuServerName, MinVersion: tls.VersionTLS12}),
		emuKeys:   func() (awsKeys, error) { return readEmulatorKeys(filepath.Join(*emuDir, "emulator_credentials")) },
		kubeURL:   *kubeURL,
		kubeHTTP:  httpClient(&tls.Config{RootCAs: kubePool, MinVersion: tls.VersionTLS12}),
		kubeToken: func() (string, error) { return readTrimmed(*kubeToken) },
		now:       time.Now,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("/v1/credentials", b.handleCredentials)
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("podidentity: listening on %s, cluster %s, expiry cap %s", *listen, *cluster, expiryCap)
	log.Fatal(srv.ListenAndServe())
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func defaultKubeAPI() string {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return "https://kubernetes.default.svc"
	}
	return "https://" + hostPort(host, port)
}

func hostPort(host, port string) string {
	if port == "" {
		port = "443"
	}
	for _, c := range host {
		if c == ':' { // IPv6
			return "[" + host + "]:" + port
		}
	}
	return host + ":" + port
}

func httpClient(cfg *tls.Config) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr, Timeout: 20 * time.Second}
}

func poolFromFile(path string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate in %s", path)
	}
	return pool, nil
}

func waitFor(d time.Duration, files ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	for _, f := range files {
		for {
			if _, err := os.Stat(f); err == nil {
				break
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s does not exist after %s", f, d)
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	return nil
}
