package kubernetes

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// In-cluster service account paths.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// RestConfig is the minimal connection configuration for the Kubernetes API.
type RestConfig struct {
	Server    string
	Namespace string // namespace from the kubeconfig context or service account
	TLS       *tls.Config
	// Token is a static bearer token; TokenFile is re-read on every request
	// (projected service account tokens rotate).
	Token     string
	TokenFile string
	Username  string
	Password  string
}

type kubeconfig struct {
	CurrentContext string `yaml:"current-context"`
	Clusters       []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthority     string `yaml:"certificate-authority"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
			InsecureSkipTLSVerify    bool   `yaml:"insecure-skip-tls-verify"`
			TLSServerName            string `yaml:"tls-server-name"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token                 string    `yaml:"token"`
			TokenFile             string    `yaml:"tokenFile"`
			ClientCertificate     string    `yaml:"client-certificate"`
			ClientCertificateData string    `yaml:"client-certificate-data"`
			ClientKey             string    `yaml:"client-key"`
			ClientKeyData         string    `yaml:"client-key-data"`
			Username              string    `yaml:"username"`
			Password              string    `yaml:"password"`
			Exec                  yaml.Node `yaml:"exec"`
			AuthProvider          yaml.Node `yaml:"auth-provider"`
		} `yaml:"user"`
	} `yaml:"users"`
	Contexts []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
}

// LoadConfig resolves cluster configuration following client conventions:
// an explicit kubeconfig path wins; otherwise in-cluster configuration is used
// when running inside a pod; otherwise $KUBECONFIG (first entry) or
// ~/.kube/config with its current context. contextName overrides the
// kubeconfig's current-context.
func LoadConfig(kubeconfigPath, contextName string) (*RestConfig, error) {
	if kubeconfigPath != "" {
		return LoadKubeconfig(kubeconfigPath, contextName)
	}
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		if _, err := os.Stat(filepath.Join(serviceAccountDir, "token")); err == nil {
			return InClusterConfig()
		}
	}
	path := ""
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		path = filepath.SplitList(kc)[0]
	} else if home, err := os.UserHomeDir(); err == nil {
		path = filepath.Join(home, ".kube", "config")
	}
	if path == "" {
		return nil, errors.New("kubernetes: no kubeconfig found and not running in a cluster")
	}
	return LoadKubeconfig(path, contextName)
}

// InClusterConfig builds configuration from the pod's service account.
func InClusterConfig() (*RestConfig, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("kubernetes: KUBERNETES_SERVICE_HOST/PORT not set")
	}
	ca, err := os.ReadFile(filepath.Join(serviceAccountDir, "ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("kubernetes: read service account CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("kubernetes: invalid service account CA")
	}
	ns, _ := os.ReadFile(filepath.Join(serviceAccountDir, "namespace"))
	return &RestConfig{
		Server:    "https://" + net.JoinHostPort(host, port),
		Namespace: strings.TrimSpace(string(ns)),
		TLS:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TokenFile: filepath.Join(serviceAccountDir, "token"),
	}, nil
}

// LoadKubeconfig parses a kubeconfig file. Exec and auth-provider plugins are
// not supported; use a static token, token file or client certificate.
func LoadKubeconfig(path, contextName string) (*RestConfig, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the kubeconfig path comes from the operator configuration
	if err != nil {
		return nil, fmt.Errorf("kubernetes: read kubeconfig: %w", err)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal(data, &kc); err != nil {
		return nil, fmt.Errorf("kubernetes: parse kubeconfig: %w", err)
	}
	dir := filepath.Dir(path)
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(dir, p)
	}
	ctxName := contextName
	if ctxName == "" {
		ctxName = kc.CurrentContext
	}
	var clusterName, userName, namespace string
	found := false
	for _, c := range kc.Contexts {
		if c.Name == ctxName {
			clusterName, userName, namespace, found = c.Context.Cluster, c.Context.User, c.Context.Namespace, true
			break
		}
	}
	if !found {
		if ctxName == "" && len(kc.Contexts) == 1 {
			c := kc.Contexts[0]
			clusterName, userName, namespace = c.Context.Cluster, c.Context.User, c.Context.Namespace
		} else {
			return nil, fmt.Errorf("kubernetes: context %q not found in kubeconfig", ctxName)
		}
	}
	cfg := &RestConfig{Namespace: namespace, TLS: &tls.Config{MinVersion: tls.VersionTLS12}}
	clusterFound := false
	for _, c := range kc.Clusters {
		if c.Name != clusterName {
			continue
		}
		clusterFound = true
		cfg.Server = strings.TrimSuffix(c.Cluster.Server, "/")
		cfg.TLS.InsecureSkipVerify = c.Cluster.InsecureSkipTLSVerify
		cfg.TLS.ServerName = c.Cluster.TLSServerName
		var ca []byte
		switch {
		case c.Cluster.CertificateAuthorityData != "":
			if ca, err = base64.StdEncoding.DecodeString(c.Cluster.CertificateAuthorityData); err != nil {
				return nil, fmt.Errorf("kubernetes: decode certificate-authority-data: %w", err)
			}
		case c.Cluster.CertificateAuthority != "":
			if ca, err = os.ReadFile(resolve(c.Cluster.CertificateAuthority)); err != nil {
				return nil, fmt.Errorf("kubernetes: read certificate-authority: %w", err)
			}
		}
		if len(ca) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(ca) {
				return nil, errors.New("kubernetes: invalid cluster CA certificate")
			}
			cfg.TLS.RootCAs = pool
		}
	}
	if !clusterFound || cfg.Server == "" {
		return nil, fmt.Errorf("kubernetes: cluster %q not found in kubeconfig", clusterName)
	}
	for _, u := range kc.Users {
		if u.Name != userName {
			continue
		}
		if !u.User.Exec.IsZero() || !u.User.AuthProvider.IsZero() {
			if u.User.Token == "" && u.User.TokenFile == "" && u.User.ClientCertificate == "" && u.User.ClientCertificateData == "" {
				return nil, fmt.Errorf("kubernetes: user %q uses an exec/auth-provider plugin, which is not supported; use a service account token or client certificate", userName)
			}
		}
		cfg.Token = u.User.Token
		cfg.TokenFile = resolve(u.User.TokenFile)
		cfg.Username, cfg.Password = u.User.Username, u.User.Password
		certPEM, keyPEM := []byte(nil), []byte(nil)
		if u.User.ClientCertificateData != "" {
			certPEM, _ = base64.StdEncoding.DecodeString(u.User.ClientCertificateData)
		} else if u.User.ClientCertificate != "" {
			if certPEM, err = os.ReadFile(resolve(u.User.ClientCertificate)); err != nil {
				return nil, err
			}
		}
		if u.User.ClientKeyData != "" {
			keyPEM, _ = base64.StdEncoding.DecodeString(u.User.ClientKeyData)
		} else if u.User.ClientKey != "" {
			if keyPEM, err = os.ReadFile(resolve(u.User.ClientKey)); err != nil {
				return nil, err
			}
		}
		if len(certPEM) > 0 {
			pair, err := tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, fmt.Errorf("kubernetes: client certificate: %w", err)
			}
			cfg.TLS.Certificates = []tls.Certificate{pair}
		}
	}
	return cfg, nil
}

// HTTPClient returns an HTTP/1.1 client for the API server (HTTP/1.1 keeps
// pods/exec WebSocket upgrades working on the same transport configuration).
func (c *RestConfig) HTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			TLSClientConfig:     c.TLS,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// authorize adds credentials to an outgoing request.
func (c *RestConfig) authorize(h http.Header) error {
	token := c.Token
	if c.TokenFile != "" {
		data, err := os.ReadFile(c.TokenFile)
		if err != nil {
			return fmt.Errorf("kubernetes: read token file: %w", err)
		}
		token = strings.TrimSpace(string(data))
	}
	switch {
	case token != "":
		h.Set("Authorization", "Bearer "+token)
	case c.Username != "":
		h.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Username+":"+c.Password)))
	}
	return nil
}
