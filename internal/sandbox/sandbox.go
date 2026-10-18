// Package sandbox holds the names and defaults that the task backends share:
// k8stask (Kubernetes Jobs), tasklocal (host processes), direct (the worker
// protocol) and the plugin task backends. The backends live in subpackages.
package sandbox

import (
	"strings"

	"github.com/openfactory-org/openfactory/internal/core"
)

const (
	// LabelRun labels containers/pods with the run id they serve.
	LabelRun = "openfactory.run"
	// LabelSandbox labels containers/pods with the sandbox id.
	LabelSandbox = "openfactory.sandbox"
	// LabelManagedBy marks resources created by Open Factory.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedByValue is the LabelManagedBy value.
	ManagedByValue = "openfactory"
)

// DefaultImage is the built-in task image used by the kubernetes backend
// when neither the environment nor the machine sets an image and the operator
// has not configured one (OF_SANDBOX_IMAGE / sandbox_image). It is a
// glibc image that ships sh, git, curl and coreutils.
//
// A release build sets it to the release tag with
// -ldflags "-X github.com/openfactory-org/openfactory/internal/sandbox.DefaultImage=<image>:<tag>".
// The initializer must stay a plain string literal so that -X can replace it.
var DefaultImage = "ghcr.io/openfactory-org/sandbox:latest"

// SelectImage returns the task image: environment image > machine image > def.
func SelectImage(spec core.SandboxSpec, def string) string {
	if spec.Environment != nil && strings.TrimSpace(spec.Environment.Image) != "" {
		return strings.TrimSpace(spec.Environment.Image)
	}
	if strings.TrimSpace(spec.Machine.Image) != "" {
		return strings.TrimSpace(spec.Machine.Image)
	}
	return def
}
