// Package forgeutil holds helpers shared by the code-forge and work-source
// integration packages: a small JSON REST client, webhook signature helpers,
// mention parsing and JSON error responses.
package forgeutil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultHTTPClient is used when a caller does not provide one.
var DefaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

// MaxResponseBytes bounds how much of a provider response is read.
const MaxResponseBytes = 32 << 20

// APIError is returned for non-2xx provider responses.
type APIError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	body := e.Body
	if len(body) > 512 {
		body = body[:512] + "..."
	}
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.Status, strings.TrimSpace(body))
}

// Temporary reports whether the request may succeed when retried.
func (e *APIError) Temporary() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// StatusCode extracts the HTTP status of an *APIError (0 if err is not one).
func StatusCode(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// IsNotFound reports whether err is an HTTP 404 from a provider.
func IsNotFound(err error) bool { return StatusCode(err) == http.StatusNotFound }

// Client is a minimal JSON REST client.
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
	// Authorize decorates each request with credentials.
	Authorize func(ctx context.Context, req *http.Request) error
	// Header is added to every request.
	Header http.Header
}

// URL joins the base URL and a path (which may contain a query string).
func (c *Client) URL(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return strings.TrimRight(c.BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

// Do sends a JSON request and decodes a JSON response into out (if non-nil).
// in may be nil, a []byte/string (sent verbatim), url.Values (form-encoded) or any JSON-marshalable value.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	_, err := c.DoRaw(ctx, method, path, in, out)
	return err
}

// DoRaw is Do but also returns the response headers.
func (c *Client) DoRaw(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	var body io.Reader
	contentType := ""
	switch v := in.(type) {
	case nil:
	case []byte:
		body = bytes.NewReader(v)
		contentType = "application/json"
	case string:
		body = strings.NewReader(v)
		contentType = "application/json"
	case url.Values:
		body = strings.NewReader(v.Encode())
		contentType = "application/x-www-form-urlencoded"
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(b)
		contentType = "application/json"
	}
	u := c.URL(path)
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	for k, vs := range c.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if contentType != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", contentType)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "openfactory"
	}
	req.Header.Set("User-Agent", ua)
	if c.Authorize != nil {
		if err := c.Authorize(ctx, req); err != nil {
			return nil, err
		}
	}
	hc := c.HTTP
	if hc == nil {
		hc = DefaultHTTPClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return resp.Header, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.Header, &APIError{Method: method, URL: redactURL(u), Status: resp.StatusCode, Body: string(data)}
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return resp.Header, nil
	}
	switch o := out.(type) {
	case *[]byte:
		*o = data
		return resp.Header, nil
	case *string:
		*o = string(data)
		return resp.Header, nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return resp.Header, fmt.Errorf("decode %s %s response: %w", method, redactURL(u), err)
	}
	return resp.Header, nil
}

func redactURL(u string) string {
	p, err := url.Parse(u)
	if err != nil {
		return u
	}
	p.User = nil
	return p.String()
}
