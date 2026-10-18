package runnerclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// StatusError is a response with a status code that is not 2xx.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Code is the "code" field of the body, if the body is JSON.
	Code string
	// Message is the "message" field of the body, or the body text.
	Message string
	// Ack is the "ack" field of the body, if present.
	Ack *int64
}

// Error returns a text that describes the response.
func (e *StatusError) Error() string {
	s := fmt.Sprintf("runnerclient: status %d", e.Status)
	if e.Code != "" {
		s += " code " + e.Code
	}
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// ErrGap means that the server has fewer events than the batch needs. The
// caller sends again from ack+1.
var ErrGap = errors.New("runnerclient: event gap")

// IsSuperseded reports whether err is a 409 response with the code
// session_superseded.
func IsSuperseded(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusConflict && se.Code == runnerproto.CodeSessionSuperseded
}

// IsRunCancelled reports whether err is a 410 response.
func IsRunCancelled(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusGone
}

// IsUnauthorized reports whether err is a 401 response.
func IsUnauthorized(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Status == http.StatusUnauthorized
}

// IsRetryable reports whether the caller can try the request again. It is
// true for network errors, for ErrRequestTimeout and for the statuses 408, 429,
// 500, 502, 503 and 504. It is false for a nil error and for context errors.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrRequestTimeout) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var se *StatusError
	if errors.As(err, &se) {
		switch se.Status {
		case http.StatusRequestTimeout, http.StatusTooManyRequests,
			http.StatusInternalServerError, http.StatusBadGateway,
			http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	}
	return true
}
