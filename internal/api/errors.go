package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

// Error codes returned in the "code" field of every error response. They are
// stable, machine-readable identifiers; clients should branch on them rather
// than on messages.
const (
	CodeInvalidRequest           = "invalid_request"
	CodeAuthenticationRequired   = "authentication_required"
	CodeNotAuthorized            = "not_authorized"
	CodeResourceNotFound         = "resource_not_found"
	CodeConflict                 = "conflict"
	CodeOperationNotSupported    = "operation_not_supported"
	CodeFeatureNotAvailable      = "feature_not_available"
	CodeIntegrationNotConfigured = "integration_not_configured"
	CodeIntegrationDisabled      = "integration_disabled"
	CodeResourceUnavailable      = "resource_unavailable"
	CodeInternalError            = "internal_error"
	// CodeExternalAuthRequired tells that a user must link an account at an
	// outside provider first. The error has provider and auth_url.
	CodeExternalAuthRequired = "external_authentication_required"
)

// errorSpec is the static description of one error code.
type errorSpec struct {
	Status    int
	Title     string
	Retryable bool
}

// errorSpecs describes each code. A conflict is not retryable: the same
// request fails the same way until the caller changes it (another name, a
// fresh read of the resource).
var errorSpecs = map[string]errorSpec{
	CodeInvalidRequest:           {http.StatusBadRequest, "The request contains invalid or missing parameters.", false},
	CodeAuthenticationRequired:   {http.StatusUnauthorized, "A valid API key is required.", false},
	CodeNotAuthorized:            {http.StatusForbidden, "You do not have permission to perform this operation.", false},
	CodeResourceNotFound:         {http.StatusNotFound, "The referenced resource does not exist.", false},
	CodeConflict:                 {http.StatusConflict, "The request conflicts with the current state of the resource.", false},
	CodeOperationNotSupported:    {http.StatusUnprocessableEntity, "The operation is not supported for this resource or state.", false},
	CodeFeatureNotAvailable:      {http.StatusNotImplemented, "This feature is not available on this server.", false},
	CodeIntegrationNotConfigured: {http.StatusFailedDependency, "The integration setup is incomplete.", false},
	CodeIntegrationDisabled:      {http.StatusFailedDependency, "The integration is disabled.", false},
	CodeResourceUnavailable:      {http.StatusServiceUnavailable, "A required resource is temporarily unavailable.", true},
	CodeInternalError:            {http.StatusInternalServerError, "An unexpected server error occurred.", true},
	CodeExternalAuthRequired:     {http.StatusUnauthorized, "A linked account at an outside provider is required.", false},
}

// Error is an API error. Handlers return it (or any error, which is mapped)
// and the server renders it as an RFC 7807 problem document.
type Error struct {
	Code   string
	Detail string
	// Reason is an optional second code for the cause.
	Reason string
	// Provider names the outside provider (for example a forge) that the
	// error is about.
	Provider string
	// AuthURL is where the user authorizes the provider.
	AuthURL string
	// SetupURL is where an admin completes the setup.
	SetupURL string
	// Extra holds additional top-level members (e.g. "errors" for validation failures).
	Extra map[string]any
}

func (e *Error) Error() string {
	if e.Detail == "" {
		return e.Code
	}
	return e.Code + ": " + e.Detail
}

// Errf builds an *Error with a formatted detail message.
func Errf(code, format string, args ...any) *Error {
	return &Error{Code: code, Detail: fmt.Sprintf(format, args...)}
}

func errInvalid(format string, args ...any) *Error { return Errf(CodeInvalidRequest, format, args...) }
func errNotFound(what string) *Error               { return Errf(CodeResourceNotFound, "%s not found", what) }
func errForbidden(format string, args ...any) *Error {
	return Errf(CodeNotAuthorized, format, args...)
}

// validationError reports definition validation failures with their file/line details.
func validationError(detail string, errs []core.ValidationError) *Error {
	if errs == nil {
		errs = []core.ValidationError{}
	}
	return &Error{Code: CodeInvalidRequest, Detail: detail, Extra: map[string]any{"errors": errs}}
}

// Problem is the JSON body of every error response. It follows RFC 7807 and
// additionally carries the platform-wide {"error":{"code","message"}} member.
type Problem struct {
	Type      string        `json:"type"`
	Title     string        `json:"title"`
	Status    int           `json:"status"`
	Detail    string        `json:"detail,omitempty"`
	Instance  string        `json:"instance,omitempty"`
	Code      string        `json:"code"`
	Error     ProblemDetail `json:"error"`
	Retryable bool          `json:"retryable"`
	// RequestID is the ID of the request. It is also in the
	// X-Request-Id response header.
	RequestID string `json:"request_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Provider  string `json:"provider,omitempty"`
	AuthURL   string `json:"auth_url,omitempty"`
	SetupURL  string `json:"setup_url,omitempty"`
}

// ProblemDetail is the nested error member.
type ProblemDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// toAPIError maps any error to an *Error.
func toAPIError(err error) (*Error, bool) {
	var ae *Error
	if errors.As(err, &ae) {
		return ae, true
	}
	switch {
	case core.IsExternalAuthRequired(err):
		return externalAuthError(err), true
	case errors.Is(err, core.ErrNotFound):
		return &Error{Code: CodeResourceNotFound, Detail: cleanCoreMessage(err, core.ErrNotFound)}, true
	case errors.Is(err, core.ErrConflict):
		return &Error{Code: CodeConflict, Detail: cleanCoreMessage(err, core.ErrConflict)}, true
	case errors.Is(err, core.ErrInvalid):
		return &Error{Code: CodeInvalidRequest, Detail: cleanCoreMessage(err, core.ErrInvalid)}, true
	case errors.Is(err, core.ErrUnauthorized):
		return &Error{Code: CodeAuthenticationRequired, Detail: cleanCoreMessage(err, core.ErrUnauthorized)}, true
	case errors.Is(err, core.ErrForbidden):
		return &Error{Code: CodeNotAuthorized, Detail: cleanCoreMessage(err, core.ErrForbidden)}, true
	case errors.Is(err, context.DeadlineExceeded):
		return &Error{Code: CodeResourceUnavailable, Detail: "request timed out"}, true
	}
	return &Error{Code: CodeInternalError}, false
}

// authProvider is implemented by an error that names the outside provider
// of core.ErrExternalAuthRequired.
type authProvider interface{ AuthProvider() string }

// externalAuthError maps core.ErrExternalAuthRequired to the API error. The
// provider comes from the error when it names one. The auth URL is the
// path where the user stores a personal token for that provider.
func externalAuthError(err error) *Error {
	ae := &Error{Code: CodeExternalAuthRequired, Detail: cleanCoreMessage(err, core.ErrUnauthorized), Reason: "no_linked_account"}
	var ap authProvider
	if errors.As(err, &ap) {
		ae.Provider = strings.ToLower(ap.AuthProvider())
	}
	ae.AuthURL = BasePath + "/me/forge-tokens"
	if ae.Provider != "" {
		ae.AuthURL += "/" + ae.Provider
	}
	return ae
}

// cleanCoreMessage strips a redundant sentinel text ("not found: ") from a wrapped error.
func cleanCoreMessage(err, sentinel error) string {
	msg := err.Error()
	if msg == sentinel.Error() {
		return ""
	}
	msg = strings.TrimPrefix(msg, sentinel.Error()+": ")
	msg = strings.TrimSuffix(msg, ": "+sentinel.Error())
	return msg
}

// writeError renders err as a problem document.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	ae, known := toAPIError(err)
	if !known && log != nil {
		log.ErrorContext(r.Context(), "api: internal error", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeProblem(w, r, ae)
}

func writeProblem(w http.ResponseWriter, r *http.Request, ae *Error) {
	spec, ok := errorSpecs[ae.Code]
	if !ok {
		spec = errorSpecs[CodeInternalError]
	}
	msg := spec.Title
	if ae.Detail != "" {
		msg = spec.Title + " (" + ae.Detail + ")"
	}
	p := Problem{
		Type:      "/errors/" + strings.ReplaceAll(ae.Code, "_", "-"),
		Title:     spec.Title,
		Status:    spec.Status,
		Detail:    ae.Detail,
		Code:      ae.Code,
		Error:     ProblemDetail{Code: ae.Code, Message: msg},
		Retryable: spec.Retryable,
		Reason:    ae.Reason,
		Provider:  ae.Provider,
		AuthURL:   ae.AuthURL,
		SetupURL:  ae.SetupURL,
	}
	if r != nil {
		p.Instance = r.URL.Path
		p.RequestID = requestID(r)
	}
	body := map[string]any{}
	raw, _ := json.Marshal(p)
	_ = json.Unmarshal(raw, &body)
	for k, v := range ae.Extra {
		if _, exists := body[k]; !exists {
			body[k] = v
		}
	}
	w.Header().Set("Content-Type", "application/problem+json")
	if spec.Status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="openfactory"`)
	}
	w.WriteHeader(spec.Status)
	_ = json.NewEncoder(w).Encode(body)
}

type requestIDKey struct{}

func requestID(r *http.Request) string {
	if v, ok := r.Context().Value(requestIDKey{}).(string); ok {
		return v
	}
	return r.Header.Get("X-Request-Id")
}
