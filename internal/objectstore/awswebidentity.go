package objectstore

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	envRoleARN          = "AWS_ROLE_ARN"
	envWebIdentityToken = "AWS_WEB_IDENTITY_TOKEN_FILE" //nolint:gosec // the name of an environment variable, not a credential
	envRoleSessionName  = "AWS_ROLE_SESSION_NAME"
	envSTSEndpoint      = "AWS_ENDPOINT_URL_STS"
	maxSTSBody          = 64 << 10
)

// webIdentityAWSCreds exchanges a projected service account token for role
// credentials with STS AssumeRoleWithWebIdentity. The call is not signed:
// the token is the proof.
type webIdentityAWSCreds struct {
	endpoint    string // https://sts.<region>.amazonaws.com/
	roleARN     string
	tokenFile   string
	sessionName string
	readFile    func(string) ([]byte, error)
	client      *http.Client
}

// newWebIdentityAWSCreds returns nil, nil when AWS_ROLE_ARN or
// AWS_WEB_IDENTITY_TOKEN_FILE is not set.
func newWebIdentityAWSCreds(opts Options, region string, readFile func(string) ([]byte, error)) (awsCredSource, error) {
	role, file := opts.getenv(envRoleARN), opts.getenv(envWebIdentityToken)
	if role == "" && file == "" {
		return nil, nil
	}
	if role == "" || file == "" {
		return nil, fmt.Errorf("objectstore: web identity needs both %s and %s", envRoleARN, envWebIdentityToken)
	}
	name := opts.getenv(envRoleSessionName)
	if name == "" {
		name = "openfactory-" + strconv.FormatInt(opts.now().Unix(), 10)
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: containerCredsTimeout}
	}
	endpoint := opts.getenv(envSTSEndpoint)
	if endpoint == "" {
		endpoint = "https://sts." + region + ".amazonaws.com/"
	}
	w := &webIdentityAWSCreds{
		endpoint:    endpoint,
		roleARN:     role,
		tokenFile:   file,
		sessionName: name,
		readFile:    readFile,
		client:      client,
	}
	return &refreshingAWSCreds{fetch: w.fetch, now: opts.now}, nil
}

func (w *webIdentityAWSCreds) fetch(ctx context.Context) (awsCreds, time.Time, error) {
	// Read the file each time: the kubelet rotates the projected token.
	b, err := w.readFile(w.tokenFile)
	if err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: read %s: %w", envWebIdentityToken, err)
	}
	form := url.Values{
		"Action":           {"AssumeRoleWithWebIdentity"},
		"Version":          {"2011-06-15"},
		"RoleArn":          {w.roleARN},
		"RoleSessionName":  {w.sessionName},
		"WebIdentityToken": {strings.TrimSpace(string(b))},
	}
	ctx, cancel := context.WithTimeout(ctx, containerCredsTimeout)
	defer cancel()
	// POST keeps the token out of URLs and access logs.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return awsCreds{}, time.Time{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := w.client.Do(req)
	if err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: sts AssumeRoleWithWebIdentity: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSTSBody))
	if err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: sts AssumeRoleWithWebIdentity: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Code    string `xml:"Error>Code"`
			Message string `xml:"Error>Message"`
		}
		_ = xml.Unmarshal(body, &e)
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: sts AssumeRoleWithWebIdentity: status %d: %s %s", resp.StatusCode, e.Code, e.Message)
	}
	var out struct {
		Creds struct {
			AccessKeyID     string `xml:"AccessKeyId"`
			SecretAccessKey string `xml:"SecretAccessKey"`
			SessionToken    string `xml:"SessionToken"`
			Expiration      string `xml:"Expiration"`
		} `xml:"AssumeRoleWithWebIdentityResult>Credentials"`
	}
	if err := xml.Unmarshal(body, &out); err != nil {
		return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: sts AssumeRoleWithWebIdentity: %w", err)
	}
	c := out.Creds
	if c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return awsCreds{}, time.Time{}, errors.New("objectstore: sts AssumeRoleWithWebIdentity: response has no credentials")
	}
	var expiry time.Time
	if c.Expiration != "" {
		if expiry, err = time.Parse(time.RFC3339, c.Expiration); err != nil {
			return awsCreds{}, time.Time{}, fmt.Errorf("objectstore: sts AssumeRoleWithWebIdentity: bad Expiration %q", c.Expiration)
		}
	}
	return awsCreds{c.AccessKeyID, c.SecretAccessKey, c.SessionToken}, expiry, nil
}
