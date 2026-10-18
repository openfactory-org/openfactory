package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	podIdentityAudience = "pods.eks.amazonaws.com"
	tokenTTLSeconds     = 600 // the smallest TokenRequest lifetime
	maxBody             = 1 << 20
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

type broker struct {
	cluster   string
	region    string
	stsHost   string
	expiryCap time.Duration

	emuURL  string
	emuHTTP *http.Client
	emuKeys func() (awsKeys, error)

	kubeURL   string
	kubeHTTP  *http.Client
	kubeToken func() (string, error)

	now func() time.Time
}

// refusal is an error with the HTTP status that answers it.
type refusal struct {
	status int
	code   string
	msg    string
}

func (e *refusal) Error() string { return e.msg }

func refuse(status int, code, format string, a ...any) *refusal {
	return &refusal{status, code, fmt.Sprintf(format, a...)}
}

type credentialsResponse struct {
	AccessKeyID     string `json:"AccessKeyId"`
	SecretAccessKey string `json:"SecretAccessKey"`
	Token           string `json:"Token"`
	Expiration      string `json:"Expiration"`
}

func (b *broker) handleCredentials(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, refuse(http.StatusMethodNotAllowed, "MethodNotAllowed", "use GET"))
		return
	}
	token := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(token) > 7 && strings.EqualFold(token[:7], "Bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	ns, sa := "-", "-"
	if token == "" {
		b.fail(w, ns, sa, refuse(http.StatusBadRequest, "InvalidRequest", "the Authorization header has no token"))
		return
	}
	ctx := r.Context()
	var err error
	if ns, sa, err = b.review(ctx, token); err != nil {
		b.fail(w, "-", "-", err)
		return
	}
	creds, role, err := b.assume(ctx, ns, sa)
	if err != nil {
		b.fail(w, ns, sa, err)
		return
	}
	creds.Expiration = b.capExpiry(creds.Expiration)
	log.Printf("issued ns=%s sa=%s role=%s expires=%s", ns, sa, role, creds.Expiration)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(creds)
}

// capExpiry returns min(exp, now+cap) in RFC 3339 when a cap is set.
func (b *broker) capExpiry(exp string) string {
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return exp
	}
	if b.expiryCap > 0 {
		if limit := b.now().Add(b.expiryCap); t.After(limit) {
			t = limit
		}
	}
	return t.UTC().Format(time.RFC3339)
}

func (b *broker) fail(w http.ResponseWriter, ns, sa string, err error) {
	var ref *refusal
	if !errors.As(err, &ref) {
		ref = refuse(http.StatusInternalServerError, "InternalFailure", "%v", err)
	}
	log.Printf("refused ns=%s sa=%s reason=%s", ns, sa, oneLine(ref.msg))
	writeError(w, ref)
}

func writeError(w http.ResponseWriter, e *refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	json.NewEncoder(w).Encode(map[string]string{"code": e.code, "message": e.msg})
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// review checks the token with a TokenReview in the real cluster.
func (b *broker) review(ctx context.Context, token string) (ns, sa string, err error) {
	own, err := b.kubeToken()
	if err != nil {
		return "", "", fmt.Errorf("read the broker's own service account token: %w", err)
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenReview",
		"spec":       map[string]any{"token": token, "audiences": []string{podIdentityAudience}},
	})
	var out struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			Error         string   `json:"error"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	status, err := doJSON(ctx, b.kubeHTTP, http.MethodPost, b.kubeURL+"/apis/authentication.k8s.io/v1/tokenreviews",
		http.Header{"Authorization": {"Bearer " + own}}, body, &out)
	if err != nil {
		return "", "", fmt.Errorf("TokenReview: %w", err)
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", "", fmt.Errorf("TokenReview: the Kubernetes API answered %d (does the broker's service account have the role system:auth-delegator?)", status)
	}
	if !out.Status.Authenticated {
		return "", "", refuse(http.StatusForbidden, "InvalidToken", "the token is not valid for the audience %s", podIdentityAudience)
	}
	if !contains(out.Status.Audiences, podIdentityAudience) {
		return "", "", refuse(http.StatusForbidden, "InvalidToken", "the token does not have the audience %s", podIdentityAudience)
	}
	rest, ok := strings.CutPrefix(out.Status.User.Username, "system:serviceaccount:")
	ns, sa, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || !dnsLabel.MatchString(ns) || !dnsLabel.MatchString(sa) {
		return "", "", refuse(http.StatusForbidden, "InvalidToken", "the token is not a service account token")
	}
	return ns, sa, nil
}

func contains(l []string, s string) bool {
	for _, v := range l {
		if v == s {
			return true
		}
	}
	return false
}

// assume gets role credentials for the service account from the emulator.
func (b *broker) assume(ctx context.Context, ns, sa string) (credentialsResponse, string, error) {
	keys, err := b.emuKeys()
	if err != nil {
		return credentialsResponse{}, "", fmt.Errorf("read the emulator keys: %w", err)
	}
	eks := signer{keys, b.region, "eks"}
	endpoint, ca, err := b.describeCluster(ctx, eks)
	if err != nil {
		return credentialsResponse{}, "", err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return credentialsResponse{}, "", errors.New("DescribeCluster returned no usable CA")
	}
	api := httpClient(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	bearer := http.Header{"Authorization": {"Bearer " + b.eksToken(keys)}}

	// Namespace and service account: create them when missing (409 is fine).
	for _, c := range []struct{ path, kind, name string }{
		{"/api/v1/namespaces", "Namespace", ns},
		{"/api/v1/namespaces/" + ns + "/serviceaccounts", "ServiceAccount", sa},
	} {
		body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": c.kind, "metadata": map[string]any{"name": c.name}})
		status, err := doJSON(ctx, api, http.MethodPost, endpoint+c.path, bearer, body, nil)
		if err != nil {
			return credentialsResponse{}, "", fmt.Errorf("create %s in the emulated cluster: %w", c.kind, err)
		}
		if status != http.StatusCreated && status != http.StatusOK && status != http.StatusConflict {
			return credentialsResponse{}, "", fmt.Errorf("create %s in the emulated cluster: status %d", c.kind, status)
		}
	}

	var tr struct {
		Status struct {
			Token string `json:"token"`
		} `json:"status"`
	}
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "authentication.k8s.io/v1",
		"kind":       "TokenRequest",
		"spec":       map[string]any{"audiences": []string{podIdentityAudience}, "expirationSeconds": tokenTTLSeconds},
	})
	status, err := doJSON(ctx, api, http.MethodPost, endpoint+"/api/v1/namespaces/"+ns+"/serviceaccounts/"+sa+"/token", bearer, body, &tr)
	if err != nil {
		return credentialsResponse{}, "", fmt.Errorf("TokenRequest in the emulated cluster: %w", err)
	}
	if status != http.StatusCreated && status != http.StatusOK || tr.Status.Token == "" {
		return credentialsResponse{}, "", fmt.Errorf("TokenRequest in the emulated cluster: status %d", status)
	}

	// eks-auth:AssumeRoleForPodIdentity.
	auth := signer{keys, b.region, "eks-auth"}
	reqBody, _ := json.Marshal(map[string]string{"token": tr.Status.Token})
	u := b.emuURL + "/clusters/" + url.PathEscape(b.cluster) + "/assume-role-for-pod-identity"
	var out struct {
		Credentials struct {
			AccessKeyID     string `json:"accessKeyId"`
			SecretAccessKey string `json:"secretAccessKey"`
			SessionToken    string `json:"sessionToken"`
			Expiration      any    `json:"expiration"`
		} `json:"credentials"`
		AssumedRoleUser struct {
			Arn string `json:"arn"`
		} `json:"assumedRoleUser"`
		Message string `json:"message"`
	}
	status, err = b.emuJSON(ctx, auth, http.MethodPost, u, reqBody, &out)
	if err != nil {
		return credentialsResponse{}, "", fmt.Errorf("AssumeRoleForPodIdentity: %w", err)
	}
	switch {
	case status >= 500:
		return credentialsResponse{}, "", fmt.Errorf("AssumeRoleForPodIdentity: the emulator answered %d", status)
	case status >= 400:
		msg := out.Message
		if msg == "" {
			msg = fmt.Sprintf("status %d", status)
		}
		return credentialsResponse{}, "", refuse(http.StatusForbidden, "AccessDenied", "no credentials for the service account: %s", msg)
	}
	c := out.Credentials
	exp, err := parseExpiration(c.Expiration)
	if err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return credentialsResponse{}, "", errors.New("AssumeRoleForPodIdentity returned an incomplete answer")
	}
	return credentialsResponse{
		AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, Token: c.SessionToken,
		Expiration: exp.UTC().Format(time.RFC3339),
	}, out.AssumedRoleUser.Arn, nil
}

// parseExpiration reads a JSON timestamp: epoch seconds (the EKS wire format)
// or an RFC 3339 string.
func parseExpiration(v any) (time.Time, error) {
	switch x := v.(type) {
	case float64:
		sec := int64(x)
		return time.Unix(sec, int64((x-float64(sec))*1e9)), nil
	case string:
		return time.Parse(time.RFC3339, x)
	}
	return time.Time{}, errors.New("no expiration")
}

func (b *broker) describeCluster(ctx context.Context, s signer) (endpoint string, ca []byte, err error) {
	var out struct {
		Cluster struct {
			Status               string `json:"status"`
			Endpoint             string `json:"endpoint"`
			CertificateAuthority struct {
				Data string `json:"data"`
			} `json:"certificateAuthority"`
		} `json:"cluster"`
	}
	status, err := b.emuJSON(ctx, s, http.MethodGet, b.emuURL+"/clusters/"+url.PathEscape(b.cluster), nil, &out)
	if err != nil {
		return "", nil, fmt.Errorf("DescribeCluster: %w", err)
	}
	if status != http.StatusOK {
		return "", nil, fmt.Errorf("DescribeCluster %s: the emulator answered %d (is the EKS cluster created?)", b.cluster, status)
	}
	if out.Cluster.Status != "ACTIVE" {
		return "", nil, fmt.Errorf("the cluster %s is %s, not ACTIVE", b.cluster, out.Cluster.Status)
	}
	ca, err = base64.StdEncoding.DecodeString(out.Cluster.CertificateAuthority.Data)
	if err != nil || out.Cluster.Endpoint == "" {
		return "", nil, errors.New("DescribeCluster returned no endpoint or CA")
	}
	return strings.TrimRight(out.Cluster.Endpoint, "/"), ca, nil
}

// eksToken makes the bearer token of "aws eks get-token": "k8s-aws-v1." and
// the base64url form of a presigned STS GetCallerIdentity URL that signs the
// header x-k8s-aws-id.
func (b *broker) eksToken(keys awsKeys) string {
	u := &url.URL{Scheme: "https", Host: b.stsHost, Path: "/", RawQuery: "Action=GetCallerIdentity&Version=2011-06-15"}
	s := signer{keys, b.region, "sts"}
	presigned := s.Presign(http.MethodGet, u, http.Header{"X-K8s-Aws-Id": {b.cluster}}, b.now(), 60*time.Second)
	return "k8s-aws-v1." + base64.RawURLEncoding.EncodeToString([]byte(presigned))
}

// emuJSON sends a signed JSON request to the emulator.
func (b *broker) emuJSON(ctx context.Context, s signer, method, u string, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	s.Sign(req, sha256Hex(body), b.now())
	return send(b.emuHTTP, req, out)
}

func doJSON(ctx context.Context, c *http.Client, method, u string, h http.Header, body []byte, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return send(c, req, out)
}

func send(c *http.Client, req *http.Request, out any) (int, error) {
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, err
	}
	if out != nil && len(data) > 0 {
		// A non-JSON error body leaves out empty; the status tells the story.
		_ = json.Unmarshal(data, out)
	}
	return resp.StatusCode, nil
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	return strings.TrimSpace(string(b)), err
}

// readEmulatorKeys reads the emulator's key file of this run (an INI file).
func readEmulatorKeys(path string) (awsKeys, error) {
	f, err := os.Open(path)
	if err != nil {
		return awsKeys{}, err
	}
	defer f.Close()
	var k awsKeys
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		name, val, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "aws_access_key_id":
			k.AccessKeyID = strings.TrimSpace(val)
		case "aws_secret_access_key":
			k.SecretAccessKey = strings.TrimSpace(val)
		case "aws_session_token":
			k.SessionToken = strings.TrimSpace(val)
		}
	}
	if k.AccessKeyID == "" || k.SecretAccessKey == "" {
		return awsKeys{}, fmt.Errorf("no keys in %s", path)
	}
	return k, sc.Err()
}
