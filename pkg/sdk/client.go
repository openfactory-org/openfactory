// Package sdk is the Go client for the Open Factory REST API.
//
//	client := sdk.NewClient("https://factories.example.com", os.Getenv("OF_API_KEY"))
//	for f, err := range client.Factories.List(ctx, &sdk.FactoryListOptions{Search: "payments"}).All(ctx) {
//		if err != nil { return err }
//		fmt.Println(f.UID, f.Name)
//	}
//	run, err := client.Factories.Dispatch(ctx, factoryUID, sdk.DispatchParams{Prompt: "Fix the flaky test"})
//
// The client retries rate-limited (429) and transient server errors (5xx)
// with exponential backoff, honors Retry-After, auto-paginates list
// endpoints, and streams run events over Server-Sent Events.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Version is the SDK version sent in the User-Agent header.
const Version = "1.0.0"

// Client talks to one Open Factory server.
type Client struct {
	baseURL    *url.URL
	apiKey     string
	httpClient *http.Client
	maxRetries int
	minBackoff time.Duration
	maxBackoff time.Duration
	userAgent  string

	Factories    *FactoriesService
	Definitions  *DefinitionsService
	Runs         *RunsService
	WorkItems    *WorkItemsService
	Inbox        *InboxService
	Webhooks     *WebhooksService
	Secrets      *SecretsService
	Environments *EnvironmentsService
	MCPServers   *MCPServersService
	APIKeys      *APIKeysService
	Users        *UsersService
	Integrations *IntegrationsService
	Measure      *MeasureService
	Platform     *PlatformService
}

// Option configures a Client.
type Option func(*Client)

// WithHTTPClient sets the underlying HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.httpClient = h } }

// WithMaxRetries sets how many times a retryable request is retried (default 3; 0 disables).
func WithMaxRetries(n int) Option { return func(c *Client) { c.maxRetries = n } }

// WithBackoff sets the exponential backoff bounds (default 500ms..8s).
func WithBackoff(min, max time.Duration) Option {
	return func(c *Client) { c.minBackoff, c.maxBackoff = min, max }
}

// WithUserAgent prefixes the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua + " " + c.userAgent }
}

// NewClient returns a client for baseURL (the server root, e.g.
// https://factories.example.com; a trailing /api/v1 is accepted).
func NewClient(baseURL, apiKey string, opts ...Option) *Client {
	base := strings.TrimRight(baseURL, "/")
	base = strings.TrimSuffix(base, "/api/v1")
	u, err := url.Parse(base + "/api/v1/")
	if err != nil {
		u = &url.URL{Scheme: "http", Host: "invalid", Path: "/api/v1/"}
	}
	c := &Client{
		baseURL: u, apiKey: apiKey, httpClient: &http.Client{Timeout: 0},
		maxRetries: 3, minBackoff: 500 * time.Millisecond, maxBackoff: 8 * time.Second,
		userAgent: "openfactory-go/" + Version,
	}
	for _, o := range opts {
		o(c)
	}
	c.Factories = &FactoriesService{c}
	c.Definitions = &DefinitionsService{c}
	c.Runs = &RunsService{c}
	c.WorkItems = &WorkItemsService{c}
	c.Inbox = &InboxService{c}
	c.Webhooks = &WebhooksService{c}
	c.Secrets = &SecretsService{c}
	c.Environments = &EnvironmentsService{c}
	c.MCPServers = &MCPServersService{c}
	c.APIKeys = &APIKeysService{c}
	c.Users = &UsersService{c}
	c.Integrations = &IntegrationsService{c}
	c.Measure = &MeasureService{c}
	c.Platform = &PlatformService{c}
	return c
}

// NewClientFromEnv builds a client from OF_API_URL (default
// http://localhost:8080) and OF_API_KEY.
func NewClientFromEnv(opts ...Option) *Client {
	base := os.Getenv("OF_API_URL")
	if base == "" {
		base = os.Getenv("OF_PUBLIC_URL")
	}
	if base == "" {
		base = "http://localhost:8080"
	}
	return NewClient(base, os.Getenv("OF_API_KEY"), opts...)
}

// ---------- errors ----------

// Error is an API error response (RFC 7807 problem document).
type Error struct {
	StatusCode int    `json:"status"`
	Code       string `json:"code"`
	Type       string `json:"type"`
	Title      string `json:"title"`
	Detail     string `json:"detail"`
	Instance   string `json:"instance"`
	Retryable  bool   `json:"retryable"`
	// RequestID is the ID of the request, for support.
	RequestID string `json:"request_id"`
	// Reason is an optional second code for the cause.
	Reason string `json:"reason,omitempty"`
	// Provider and AuthURL are set for CodeExternalAuthRequired: the user
	// must link an account at Provider first, at AuthURL.
	Provider   string            `json:"provider,omitempty"`
	AuthURL    string            `json:"auth_url,omitempty"`
	SetupURL   string            `json:"setup_url,omitempty"`
	Errors     []ValidationError `json:"errors"`
	RetryAfter time.Duration     `json:"-"`
	Body       []byte            `json:"-"`
}

func (e *Error) Error() string {
	msg := e.Title
	if e.Detail != "" {
		msg += " (" + e.Detail + ")"
	}
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("openfactory: %d %s: %s", e.StatusCode, e.Code, msg)
}

// Error codes.
const (
	CodeInvalidRequest         = "invalid_request"
	CodeAuthenticationRequired = "authentication_required"
	CodeNotAuthorized          = "not_authorized"
	CodeResourceNotFound       = "resource_not_found"
	CodeConflict               = "conflict"
	CodeOperationNotSupported  = "operation_not_supported"
	CodeFeatureNotAvailable    = "feature_not_available"
	CodeResourceUnavailable    = "resource_unavailable"
	CodeInternalError          = "internal_error"
	CodeExternalAuthRequired   = "external_authentication_required"
)

// ErrorCode returns the API error code of err ("" when err is not an *Error).
func ErrorCode(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// IsNotFound reports a resource_not_found error.
func IsNotFound(err error) bool { return ErrorCode(err) == CodeResourceNotFound }

// IsConflict reports a conflict error.
func IsConflict(err error) bool { return ErrorCode(err) == CodeConflict }

// IsUnauthorized reports an authentication_required error.
func IsUnauthorized(err error) bool { return ErrorCode(err) == CodeAuthenticationRequired }

// IsForbidden reports a not_authorized error.
func IsForbidden(err error) bool { return ErrorCode(err) == CodeNotAuthorized }

// IsInvalid reports an invalid_request error.
func IsInvalid(err error) bool { return ErrorCode(err) == CodeInvalidRequest }

func parseError(res *http.Response, body []byte) *Error {
	e := &Error{StatusCode: res.StatusCode, Body: body}
	_ = json.Unmarshal(body, e)
	if e.Code == "" {
		// Accept the nested {"error":{"code","message"}} form too.
		var nested struct {
			Error struct{ Code, Message string } `json:"error"`
		}
		if json.Unmarshal(body, &nested) == nil && nested.Error.Code != "" {
			e.Code, e.Title = nested.Error.Code, nested.Error.Message
		}
	}
	e.StatusCode = res.StatusCode
	if e.Code == "" {
		switch {
		case res.StatusCode == 401:
			e.Code = CodeAuthenticationRequired
		case res.StatusCode == 403:
			e.Code = CodeNotAuthorized
		case res.StatusCode == 404:
			e.Code = CodeResourceNotFound
		case res.StatusCode == 409:
			e.Code = CodeConflict
		case res.StatusCode >= 500:
			e.Code = CodeInternalError
		default:
			e.Code = CodeInvalidRequest
		}
	}
	if ra := res.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(ra); err == nil {
			e.RetryAfter = time.Duration(secs) * time.Second
		} else if t, err := http.ParseTime(ra); err == nil {
			e.RetryAfter = time.Until(t)
		}
	}
	return e
}

// ---------- transport ----------

func (c *Client) url(path string, q url.Values) string {
	u := *c.baseURL
	rel, _ := url.Parse(strings.TrimPrefix(path, "/"))
	full := u.ResolveReference(rel)
	if len(q) > 0 {
		full.RawQuery = q.Encode()
	}
	return full.String()
}

func shouldRetry(method string, status int, netErr error) bool {
	if netErr != nil {
		// Connection failures before a response: safe for idempotent methods only.
		return method != http.MethodPost && method != http.MethodPatch
	}
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable:
		return true // the request was not processed
	case http.StatusInternalServerError, http.StatusBadGateway, http.StatusGatewayTimeout:
		return method != http.MethodPost && method != http.MethodPatch
	}
	return false
}

func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	if retryAfter > 0 {
		if retryAfter > c.maxBackoff*4 {
			return c.maxBackoff * 4
		}
		return retryAfter
	}
	d := c.minBackoff << attempt
	if d <= 0 || d > c.maxBackoff {
		d = c.maxBackoff
	}
	// Full jitter in [d/2, d].
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1)) //nolint:gosec // G404: retry jitter needs no cryptographic randomness
}

// rawRequest performs a request with retries and returns the successful response.
func (c *Client) rawRequest(ctx context.Context, method, path string, q url.Values, body any, accept string, headers map[string]string) (*http.Response, error) {
	var payload []byte
	if raw, ok := body.([]byte); ok {
		payload = raw // sent as is; the caller sets Content-Type in headers
	} else if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, fmt.Errorf("openfactory: encode request: %w", err)
		}
	}
	for attempt := 0; ; attempt++ {
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.url(path, q), rdr)
		if err != nil {
			return nil, err
		}
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		req.Header.Set("User-Agent", c.userAgent)
		req.Header.Set("Accept", accept)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil || attempt >= c.maxRetries || !shouldRetry(method, 0, err) {
				return nil, err
			}
			if err := sleep(ctx, c.backoff(attempt, 0)); err != nil {
				return nil, err
			}
			continue
		}
		if res.StatusCode < 300 {
			return res, nil
		}
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		apiErr := parseError(res, data)
		if attempt < c.maxRetries && shouldRetry(method, res.StatusCode, nil) {
			if err := sleep(ctx, c.backoff(attempt, apiErr.RetryAfter)); err != nil {
				return nil, err
			}
			continue
		}
		return nil, apiErr
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Do sends a JSON request to path (relative to /api/v1) and decodes the
// response into out (which may be nil). It is exported for endpoints the SDK
// does not wrap yet.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	res, err := c.rawRequest(ctx, method, path, query, body, "application/json", nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil || res.StatusCode == http.StatusNoContent {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	if raw, ok := out.(*json.RawMessage); ok {
		data, err := io.ReadAll(res.Body)
		*raw = data
		return err
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("openfactory: decode response: %w", err)
	}
	return nil
}

func pathEscape(s string) string { return url.PathEscape(s) }
