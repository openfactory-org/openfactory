package engine

import (
	"context"
	"fmt"
	"sort"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
)

// Sources of HarnessCredential.
const (
	// CredentialManagedSecret is harness.auth with source managedSecret.
	CredentialManagedSecret = "managedSecret"
	// CredentialWorkerEnvironment is harness.auth with source workerEnvironment.
	CredentialWorkerEnvironment = "workerEnvironment"
	// CredentialDefault is the default model credential: the one secret of
	// the factory or the team whose type the harness accepts.
	CredentialDefault = "default"
	// CredentialNone means that Open Factory passes no credential. The CLI
	// uses the credentials of the sandbox.
	CredentialNone = "none"
)

// Reasons of a HarnessCredential with source none.
const (
	// CredentialReasonRunEnv: the environment of the run already holds a
	// model credential (an environment variable or an agent secret).
	CredentialReasonRunEnv = "run_environment"
	// CredentialReasonAmbiguous: more than one secret fits at the same scope.
	CredentialReasonAmbiguous = "ambiguous"
	// CredentialReasonNoSecret: no secret fits.
	CredentialReasonNoSecret = "no_secret"
)

// Scopes of a default credential.
const (
	CredentialScopeFactory = "factory"
	CredentialScopeTeam    = "team"
)

// HarnessCredential tells where the model credential of a run comes from.
// It holds secret names and never secret values.
type HarnessCredential struct {
	Source     string          `json:"source"`
	SecretName string          `json:"secret,omitempty"`
	Scope      string          `json:"scope,omitempty"`
	Type       core.SecretType `json:"type,omitempty"`
	Reason     string          `json:"reason,omitempty"`
	Candidates []string        `json:"candidates,omitempty"`
}

// IsDefault reports whether the credential is the default credential.
func (c HarnessCredential) IsDefault() bool { return c.Source == CredentialDefault }

// findDefaultCredential finds the default model credential of a harness: the
// secret whose type the harness accepts (harness.CredentialSecretTypes).
//
//   - A factory secret wins over a team secret. A team secret is not a
//     candidate when a factory secret has the same name, because the name
//     resolves to the factory secret.
//   - Exactly one fitting secret at a scope is the default.
//   - More than one fitting secret at the first scope that has any is
//     ambiguous: the result has source none, reason ambiguous and the
//     candidate names, and no secret is used.
//   - No fitting secret gives source none with reason no_secret.
func findDefaultCredential(ctx context.Context, ss core.SecretStore, teamID, factoryID string, h core.HarnessType) (HarnessCredential, error) {
	none := HarnessCredential{Source: CredentialNone, Reason: CredentialReasonNoSecret}
	if ss == nil || len(harness.CredentialSecretTypes(h)) == 0 {
		return none, nil
	}
	type scope struct{ name, factoryID string }
	scopes := []scope{{CredentialScopeTeam, ""}}
	if factoryID != "" {
		scopes = []scope{{CredentialScopeFactory, factoryID}, {CredentialScopeTeam, ""}}
	}
	shadowed := map[string]bool{}
	for _, sc := range scopes {
		list, err := ss.List(ctx, teamID, sc.factoryID)
		if err != nil {
			return none, fmt.Errorf("list %s secrets: %w", sc.name, err)
		}
		var fits []core.Secret
		for _, s := range list {
			if s.FactoryID != sc.factoryID || shadowed[s.Name] {
				continue
			}
			if harness.AcceptsCredentialType(h, s.Type) {
				fits = append(fits, s)
			}
		}
		for _, s := range list {
			if s.FactoryID == sc.factoryID {
				shadowed[s.Name] = true
			}
		}
		switch len(fits) {
		case 0:
			continue
		case 1:
			return HarnessCredential{Source: CredentialDefault, SecretName: fits[0].Name, Scope: sc.name, Type: fits[0].Type}, nil
		default:
			names := make([]string, 0, len(fits))
			for _, s := range fits {
				names = append(names, s.Name)
			}
			sort.Strings(names)
			return HarnessCredential{Source: CredentialNone, Reason: CredentialReasonAmbiguous, Scope: sc.name, Candidates: names}, nil
		}
	}
	return none, nil
}
