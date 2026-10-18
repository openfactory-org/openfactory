package runnerclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

const (
	maxErrorBody    = 64 << 10
	maxErrorMessage = 512
)

// commandsGrace is the time that a commands poll waits for the answer after
// the wait time. A variable, so that a test can make it short.
var commandsGrace = 15 * time.Second

// eventsTimeout is the deadline of one events request. A variable, so that a
// test can make it short.
var eventsTimeout = 30 * time.Second

// ErrRequestTimeout means that a request (a commands poll or an events send)
// got no answer before its own deadline. The connection can go to a replica
// that stopped without a reset, so the caller tries again. IsRetryable is
// true for it.
var ErrRequestTimeout = errors.New("runnerclient: the request got no answer in time")

// withDeadline calls f with a context that ends after d. When that deadline
// ends but ctx did not, the error wraps ErrRequestTimeout.
func withDeadline(ctx context.Context, d time.Duration, f func(context.Context) error) error {
	rctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	err := f(rctx)
	if err != nil && ctx.Err() == nil && errors.Is(rctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: %w", ErrRequestTimeout, err)
	}
	return err
}

// Options are the settings of a Client.
type Options struct {
	// BaseURL is the URL of the control plane, for example
	// https://factory.internal. A trailing slash is removed.
	BaseURL string
	// HTTPClient is the HTTP client to use. Nil means a client with no total
	// timeout.
	HTTPClient *http.Client
	// RunnerVersion is sent in the header runnerproto.HeaderRunnerVersion.
	RunnerVersion string
}

// Client sends the requests of the runner protocol to the control plane.
type Client struct {
	base    string
	http    *http.Client
	version string

	mu    sync.Mutex
	token string
}

// New makes a Client. It returns an error if BaseURL is empty or is not an
// absolute http or https URL.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		return nil, errors.New("runnerclient: BaseURL is empty")
	}
	u, err := url.Parse(o.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("runnerclient: parse BaseURL: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("runnerclient: BaseURL %q is not an absolute http or https URL", o.BaseURL)
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{
		base:    strings.TrimRight(o.BaseURL, "/"),
		http:    hc,
		version: o.RunnerVersion,
	}, nil
}

// SetToken sets the token that later requests send.
func (c *Client) SetToken(token string) {
	c.mu.Lock()
	c.token = token
	c.mu.Unlock()
}

// Token returns the token that requests send now.
func (c *Client) Token() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// call describes one request to the control plane.
type call struct {
	method string
	path   string
	query  url.Values
	token  string // empty means the session token
	body   any    // nil means no body
}

// send runs the request and returns the response for a 2xx status. For any
// other status it returns a *StatusError. The caller closes the body.
func (c *Client) send(ctx context.Context, k call) (*http.Response, error) {
	var rd io.Reader
	if k.body != nil {
		b, err := json.Marshal(k.body)
		if err != nil {
			return nil, fmt.Errorf("runnerclient: encode request: %w", err)
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + k.path
	if len(k.query) > 0 {
		u += "?" + k.query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, k.method, u, rd)
	if err != nil {
		return nil, fmt.Errorf("runnerclient: make request: %w", err)
	}
	tok := k.token
	if tok == "" {
		tok = c.Token()
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if c.version != "" {
		req.Header.Set(runnerproto.HeaderRunnerVersion, c.version)
	}
	if k.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("runnerclient: %s %s: %w", k.method, k.path, err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, statusError(resp)
	}
	return resp, nil
}

// statusError reads an error response.
func statusError(resp *http.Response) *StatusError {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	se := &StatusError{Status: resp.StatusCode}
	var eb runnerproto.ErrorBody
	if err := json.Unmarshal(raw, &eb); err == nil {
		se.Code = eb.Code
		se.Message = eb.Message
		se.Ack = eb.Ack
		return se
	}
	msg := strings.TrimSpace(string(raw))
	if len(msg) > maxErrorMessage {
		msg = msg[:maxErrorMessage]
	}
	se.Message = msg
	return se
}

// do runs the request and decodes a JSON body into out. A nil out ignores
// the body.
func (c *Client) do(ctx context.Context, k call, out any) error {
	resp, err := c.send(ctx, k)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("runnerclient: decode response of %s: %w", k.path, err)
	}
	return nil
}

// gapError turns a 409 response with an ack into an error that wraps ErrGap.
func gapError(err error) (int64, error) {
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusConflict && se.Ack != nil && se.Code != runnerproto.CodeSessionSuperseded {
		return *se.Ack, fmt.Errorf("%w: %w", ErrGap, se)
	}
	return 0, err
}

// Start sends the session start request with the bootstrap token. On success
// it keeps the session token of the response for later requests.
func (c *Client) Start(ctx context.Context, bootstrapToken string, req runnerproto.StartRequest) (runnerproto.StartResponse, error) {
	var resp runnerproto.StartResponse
	if err := req.Validate(); err != nil {
		return resp, fmt.Errorf("runnerclient: invalid start request: %w", err)
	}
	k := call{method: http.MethodPost, path: runnerproto.PathSessionStart, token: bootstrapToken, body: req}
	if err := c.do(ctx, k, &resp); err != nil {
		return runnerproto.StartResponse{}, err
	}
	c.SetToken(resp.SessionToken)
	return resp, nil
}

// Secrets gets the secrets of the session.
func (c *Client) Secrets(ctx context.Context) (runnerproto.SecretsResponse, error) {
	var resp runnerproto.SecretsResponse
	err := c.do(ctx, call{method: http.MethodGet, path: runnerproto.PathSessionSecrets}, &resp)
	return resp, err
}

// SendEvents sends a batch of events and returns the ack of the control
// plane. If the control plane has fewer events than the batch needs, it
// returns the ack of the control plane and an error that wraps ErrGap and the
// *StatusError. For every other error the ack is 0. A request without an
// answer in eventsTimeout gives an error that wraps ErrRequestTimeout.
func (c *Client) SendEvents(ctx context.Context, batch runnerproto.EventBatch) (ack int64, err error) {
	var resp runnerproto.EventAck
	k := call{method: http.MethodPost, path: runnerproto.PathSessionEvents, body: batch}
	err = withDeadline(ctx, eventsTimeout, func(ctx context.Context) error { return c.do(ctx, k, &resp) })
	if err != nil {
		return gapError(err)
	}
	return resp.Ack, nil
}

// Commands gets the commands after the sequence number after. It waits up to
// waitSeconds, limited by runnerproto.ClampWait, for a command. It returns an
// empty, non-nil slice if there is no command.
func (c *Client) Commands(ctx context.Context, after int64, waitSeconds int) ([]runnerproto.Command, error) {
	wait := runnerproto.ClampWait(waitSeconds)
	q := url.Values{}
	q.Set("after", strconv.FormatInt(after, 10))
	q.Set("wait", strconv.Itoa(wait))
	var resp runnerproto.CommandsResponse
	k := call{method: http.MethodGet, path: runnerproto.PathSessionCommands, query: q}
	if err := withDeadline(ctx, time.Duration(wait)*time.Second+commandsGrace, func(ctx context.Context) error {
		return c.do(ctx, k, &resp)
	}); err != nil {
		return nil, err
	}
	if resp.Commands == nil {
		resp.Commands = []runnerproto.Command{}
	}
	return resp.Commands, nil
}

// End sends the session end request. If the control plane has fewer events
// than req.LastEventSeq, it returns an error that wraps ErrGap and the
// *StatusError.
func (c *Client) End(ctx context.Context, req runnerproto.EndRequest) error {
	if err := req.Validate(); err != nil {
		return fmt.Errorf("runnerclient: invalid end request: %w", err)
	}
	err := c.do(ctx, call{method: http.MethodPost, path: runnerproto.PathSessionEnd, body: req}, nil)
	if err != nil {
		_, err = gapError(err)
	}
	return err
}

// RequestUpload asks the control plane for a URL to upload a file to.
func (c *Client) RequestUpload(ctx context.Context, req runnerproto.UploadRequest) (runnerproto.UploadResponse, error) {
	var resp runnerproto.UploadResponse
	err := c.do(ctx, call{method: http.MethodPost, path: runnerproto.PathSessionUploads, body: req}, &resp)
	return resp, err
}

// PutFile sends size bytes from body to the upload URL of up. It does not
// send the session token or the runner version, because the request goes to
// an object store.
func (c *Client) PutFile(ctx context.Context, up runnerproto.UploadResponse, body io.Reader, size int64) error {
	method := up.Method
	if method == "" {
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(ctx, method, up.URL, body)
	if err != nil {
		return fmt.Errorf("runnerclient: make upload request: %w", err)
	}
	for k, v := range up.Headers {
		req.Header.Set(k, v)
	}
	req.ContentLength = size
	if size == 0 {
		req.Body = http.NoBody
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("runnerclient: upload file: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return statusError(resp)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// Downloads gets the download URLs for files of a kind. The id is optional.
func (c *Client) Downloads(ctx context.Context, kind string, id string) (runnerproto.DownloadsResponse, error) {
	q := url.Values{}
	q.Set("kind", kind)
	if id != "" {
		q.Set("id", id)
	}
	var resp runnerproto.DownloadsResponse
	err := c.do(ctx, call{method: http.MethodGet, path: runnerproto.PathSessionDownloads, query: q}, &resp)
	return resp, err
}

// GitCredential gets a credential for a git repository.
func (c *Client) GitCredential(ctx context.Context, req runnerproto.GitCredentialRequest) (runnerproto.GitCredentialResponse, error) {
	var resp runnerproto.GitCredentialResponse
	err := c.do(ctx, call{method: http.MethodPost, path: runnerproto.PathGitCredential, body: req}, &resp)
	return resp, err
}

// CloudToken gets a cloud identity token for an audience. A ttlSeconds of 0
// asks for the default life.
func (c *Client) CloudToken(ctx context.Context, audience string, ttlSeconds int) (runnerproto.CloudTokenResponse, error) {
	var resp runnerproto.CloudTokenResponse
	req := runnerproto.CloudTokenRequest{Audience: audience, TTLSeconds: ttlSeconds}
	err := c.do(ctx, call{method: http.MethodPost, path: runnerproto.PathCloudToken, body: req}, &resp)
	return resp, err
}

// RefreshToken asks for a new session token. On success it keeps the new
// token for later requests.
func (c *Client) RefreshToken(ctx context.Context) (runnerproto.TokenResponse, error) {
	var resp runnerproto.TokenResponse
	if err := c.do(ctx, call{method: http.MethodPost, path: runnerproto.PathSessionToken}, &resp); err != nil {
		return runnerproto.TokenResponse{}, err
	}
	c.SetToken(resp.SessionToken)
	return resp, nil
}

// Conversation gets the conversation of the run.
func (c *Client) Conversation(ctx context.Context) (runnerproto.Conversation, error) {
	var resp runnerproto.Conversation
	err := c.do(ctx, call{method: http.MethodGet, path: runnerproto.PathConversation}, &resp)
	return resp, err
}
