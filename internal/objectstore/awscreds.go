package objectstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AWS credential sources of the S3 driver, in this order:
//
//  1. Options.AWSAccessKeyID / AWSSecretAccessKey / AWSSessionToken.
//  2. AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN.
//  3. Web identity (IRSA): AWS_ROLE_ARN and AWS_WEB_IDENTITY_TOKEN_FILE, with
//     an optional AWS_ROLE_SESSION_NAME. The driver calls STS
//     AssumeRoleWithWebIdentity in the region of the store, or at
//     AWS_ENDPOINT_URL_STS when it is set.
//  4. The container credentials endpoint: AWS_CONTAINER_CREDENTIALS_FULL_URI
//     or AWS_CONTAINER_CREDENTIALS_RELATIVE_URI, with the token from
//     AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE or AWS_CONTAINER_AUTHORIZATION_TOKEN.
//     EKS Pod Identity and ECS task roles set these variables.

// awsCredSource returns credentials that are valid for at least minValid.
type awsCredSource interface {
	Retrieve(ctx context.Context, minValid time.Duration) (awsCreds, error)
}

type staticAWSCreds awsCreds

func (s staticAWSCreds) Retrieve(context.Context, time.Duration) (awsCreds, error) {
	return awsCreds(s), nil
}

const (
	envContainerFullURI   = "AWS_CONTAINER_CREDENTIALS_FULL_URI"
	envContainerRelURI    = "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI"
	envContainerTokenFile = "AWS_CONTAINER_AUTHORIZATION_TOKEN_FILE" //nolint:gosec // the name of an environment variable, not a credential
	envContainerToken     = "AWS_CONTAINER_AUTHORIZATION_TOKEN"      //nolint:gosec // the name of an environment variable, not a credential
	// ecsCredentialsHost is the host of AWS_CONTAINER_CREDENTIALS_RELATIVE_URI.
	ecsCredentialsHost = "169.254.170.2"
	// awsCredsRefresh renews temporary credentials this long before they expire.
	awsCredsRefresh       = 5 * time.Minute
	containerCredsTimeout = 5 * time.Second
	maxContainerCredsBody = 64 << 10
)

// refreshingAWSCreds caches temporary credentials from fetch until
// shortly before they expire.
type refreshingAWSCreds struct {
	fetch func(context.Context) (awsCreds, time.Time, error)
	now   func() time.Time

	mu     sync.Mutex
	cached awsCreds
	expiry time.Time // zero: no expiry given
	valid  bool
}

// Retrieve returns cached credentials while they stay valid for minValid
// plus the refresh margin, and fetches new ones otherwise.
func (c *refreshingAWSCreds) Retrieve(ctx context.Context, minValid time.Duration) (awsCreds, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && (c.expiry.IsZero() || c.now().Add(minValid+awsCredsRefresh).Before(c.expiry)) {
		return c.cached, nil
	}
	creds, expiry, err := c.fetch(ctx)
	if err != nil {
		return awsCreds{}, err
	}
	c.cached, c.expiry, c.valid = creds, expiry, true
	return creds, nil
}

// containerAWSCreds reads temporary credentials from the container
// credentials endpoint.
type containerAWSCreds struct {
	endpoint  string
	tokenFile string
	token     string
	readFile  func(string) ([]byte, error)
	client    *http.Client
}

// newContainerAWSCreds returns nil, nil when no container credentials
// variable is set.
func newContainerAWSCreds(opts Options, readFile func(string) ([]byte, error)) (awsCredSource, error) {
	endpoint := opts.getenv(envContainerFullURI)
	if rel := opts.getenv(envContainerRelURI); endpoint == "" && rel != "" {
		if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}
		endpoint = "http://" + ecsCredentialsHost + rel
	}
	if endpoint == "" {
		return nil, nil
	}
	if err := checkContainerCredsURL(endpoint); err != nil {
		return nil, err
	}
	client := opts.HTTPClient
	if client == nil {
		// The endpoint is link-local or loopback: never send it through a proxy.
		client = &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: containerCredsTimeout}
	}
	c := &containerAWSCreds{
		endpoint:  endpoint,
		tokenFile: opts.getenv(envContainerTokenFile),
		token:     opts.getenv(envContainerToken),
		readFile:  readFile,
		client:    client,
	}
	return &refreshingAWSCreds{fetch: c.fetch, now: opts.now}, nil
}

// checkContainerCredsURL allows the same endpoints as the AWS SDKs: HTTPS
// anywhere, or HTTP to a loopback address, the ECS endpoint or the EKS Pod
// Identity endpoint. The token must not go to any other plain-HTTP host.
func checkContainerCredsURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("objectstore: %s is not a URL", envContainerFullURI)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
	default:
		return fmt.Errorf("objectstore: %s must use http or https", envContainerFullURI)
	}
	host := u.Hostname()
	switch host {
	case ecsCredentialsHost, "169.254.170.23", "fd00:ec2::23", "localhost":
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("objectstore: %s with http must point at a loopback address, %s or the EKS Pod Identity agent, not %q", envContainerFullURI, ecsCredentialsHost, host)
}

func (c *containerAWSCreds) fetch(ctx context.Context) (awsCreds, time.Time, error) {
	token := c.token
	if c.tokenFile != "" {
		// Read the file each time: the kubelet rotates the projected token.
		b, err := c.readFile(c.tokenFile)
		if err != nil {
			return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: read %s: %w", envContainerTokenFile, err)
		}
		token = strings.TrimSpace(string(b))
	}
	if strings.ContainsAny(token, "\r\n") {
		return awsCreds{}, time.Time{}, errors.New("objectstore: container credentials token contains a line break")
	}
	ctx, cancel := context.WithTimeout(ctx, containerCredsTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint, nil)
	if err != nil {
		return awsCreds{}, time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: container credentials: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxContainerCredsBody))
	if err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: container credentials: %w", err)
	}
	var out struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		Token           string `json:"Token"`
		Expiration      string `json:"Expiration"`
		Code            string `json:"Code"`
		Message         string `json:"Message"`
	}
	_ = json.Unmarshal(body, &out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Message
		if msg == "" {
			msg = out.Code
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: container credentials: status %d: %s", resp.StatusCode, msg)
	}
	if out.AccessKeyID == "" || out.SecretAccessKey == "" {
		return awsCreds{}, time.Time{}, errors.New("objectstore: container credentials: response has no AccessKeyId or SecretAccessKey")
	}
	var expiry time.Time
	if out.Expiration != "" {
		expiry, err = time.Parse(time.RFC3339, out.Expiration)
		if err != nil {
			return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: container credentials: bad Expiration %q", out.Expiration)
		}
	}
	return awsCreds{out.AccessKeyID, out.SecretAccessKey, out.Token}, expiry, nil
}
