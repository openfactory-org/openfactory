package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// PrincipalType is the type of a principal.
type PrincipalType string

const (
	// PrincipalUser is a person. Its ID starts with "usr_".
	PrincipalUser PrincipalType = "user"
	// PrincipalServiceAccount is a principal that is not a person, with a
	// role and API keys. Its ID starts with "sa_".
	PrincipalServiceAccount PrincipalType = "service_account"
	// PrincipalFactory is the identity of a factory. Its ID is the factory
	// ID. It uses the integration credentials of the factory.
	PrincipalFactory PrincipalType = "factory"
)

// Principal is who an action is done for. A run records two: its creator
// and the principal that it acts as (Run.Creator, Run.RunAs).
type Principal struct {
	Type PrincipalType `json:"type"`
	ID   string        `json:"id"`
	Name string        `json:"name,omitempty"`
}

// String returns "<type>:<id>", for example "user:usr_1" or
// "factory:fac_1". It is the sub claim of a session token and the run_as
// claim of a cloud identity token.
func (p *Principal) String() string {
	if p == nil || p.ID == "" {
		return ""
	}
	return string(p.Type) + ":" + p.ID
}

// CreatorID returns the ID of the creator of r, or "".
func (r *Run) CreatorID() string {
	if r == nil || r.Creator == nil {
		return ""
	}
	return r.Creator.ID
}

// IsPerson reports whether p is a user (a person).
func (p *Principal) IsPerson() bool { return p != nil && p.Type == PrincipalUser && p.ID != "" }

// UserKind values of User.Kind.
const (
	// UserKindPerson is a person. It is the default (an empty Kind).
	UserKindPerson = ""
	// UserKindServiceAccount is a service account.
	UserKindServiceAccount = "service_account"
)

// ServiceAccountIDPrefix is the ID prefix of a service account.
const ServiceAccountIDPrefix = "sa"

// IsServiceAccount reports whether u is a service account.
func (u *User) IsServiceAccount() bool { return u != nil && u.Kind == UserKindServiceAccount }

// AsPrincipal returns the principal of a user record: a user or a service
// account.
func (u *User) AsPrincipal() *Principal {
	if u == nil || u.ID == "" {
		return nil
	}
	p := &Principal{Type: PrincipalUser, ID: u.ID, Name: u.Name}
	if u.IsServiceAccount() {
		p.Type = PrincipalServiceAccount
	}
	if p.Name == "" {
		p.Name = u.Email
	}
	return p
}

// FactoryPrincipal returns the principal of a factory.
func FactoryPrincipal(f *Factory) *Principal {
	if f == nil {
		return nil
	}
	name := f.Name
	if name == "" {
		name = f.Alias
	}
	return &Principal{Type: PrincipalFactory, ID: f.ID, Name: name}
}

// Values of Automation.RunAs (factory.yaml runAs).
const (
	// RunAsFactory is the default runAs of an automation.
	RunAsFactory = "factory"
	// RunAsUserPrefix starts a runAs that names a user by email.
	RunAsUserPrefix = "user:"
	// RunAsServiceAccountPrefix starts a runAs that names a service account
	// by name.
	RunAsServiceAccountPrefix = "serviceAccount:"
)

// ParseRunAs splits a runAs value into its principal type and the email
// (user) or name (service account). An empty value is the factory.
func ParseRunAs(v string) (PrincipalType, string, error) {
	v = strings.TrimSpace(v)
	switch {
	case v == "" || v == RunAsFactory:
		return PrincipalFactory, "", nil
	case strings.HasPrefix(v, RunAsUserPrefix):
		email := strings.TrimSpace(strings.TrimPrefix(v, RunAsUserPrefix))
		if email == "" || !strings.Contains(email, "@") {
			return "", "", fmt.Errorf("%w: runAs %q: want user:<email>", ErrInvalid, v)
		}
		return PrincipalUser, email, nil
	case strings.HasPrefix(v, RunAsServiceAccountPrefix):
		name := strings.TrimSpace(strings.TrimPrefix(v, RunAsServiceAccountPrefix))
		if name == "" {
			return "", "", fmt.Errorf("%w: runAs %q: want serviceAccount:<name>", ErrInvalid, v)
		}
		return PrincipalServiceAccount, name, nil
	}
	return "", "", fmt.Errorf("%w: runAs %q: want factory, user:<email> or serviceAccount:<name>", ErrInvalid, v)
}

// ErrExternalAuthRequired means that the run_as user of a CREATOR run has
// no linked account at the forge. It wraps
// ErrUnauthorized, so the API answers 401. A run fails with
// FailureExternalAuthenticationRequired.
var ErrExternalAuthRequired = fmt.Errorf("%w: external authentication required", ErrUnauthorized)

// IsExternalAuthRequired reports whether err is ErrExternalAuthRequired.
func IsExternalAuthRequired(err error) bool { return errors.Is(err, ErrExternalAuthRequired) }

// UserDeleter is implemented by the stores. DeleteUser deletes a user
// record and each API key of that user, in one step. It returns ErrNotFound
// if the user does not exist. The API uses it to delete a service account.
type UserDeleter interface {
	DeleteUser(ctx context.Context, id string) error
}
