package runner

import (
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/harness"
	"github.com/openfactory-org/openfactory/internal/runnerproto"
)

// defaultCredentialNotUsed is the code of the diagnostic event of
// dropDefaultAuth.
const defaultCredentialNotUsed = "default_credential_not_used"

// dropDefaultAuth drops the default model credential when the environment
// of the runner (the sandbox environment, for example ANTHROPIC_API_KEY of
// the process) already holds a model credential for the harness. The
// definition sets no harness.auth in this case, and before the default
// credential existed the CLI used the sandbox credential. That stays so.
func (r *runner) dropDefaultAuth() {
	env := map[string]string{}
	for _, kv := range r.env() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env[k] = v
		}
	}
	if !harness.HasCredentialEnv(core.HarnessType(r.start.Harness.Type), env) {
		return
	}
	r.harnessAuth = nil
	msg := "the sandbox environment holds a model credential, so the run does not use the default credential secret"
	r.log.Info(msg)
	r.emit(runnerproto.EventDiagnostic, "", runnerproto.DiagnosticData{Code: defaultCredentialNotUsed, Message: msg})
}
