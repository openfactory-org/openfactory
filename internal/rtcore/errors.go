package rtcore

import "errors"

// Sentinel errors. Wrap them with fmt.Errorf("%w: ...") and compare with
// errors.Is.
var (
	ErrNotFound     = errors.New("not found")
	ErrInvalid      = errors.New("invalid")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)
