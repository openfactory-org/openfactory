// Package rtcore holds the domain types that the runtime (runner, harness
// adapters, netguard, netlock, the dev worker and the microVM worker) needs.
// It is a small split of internal/core: the runtime binary must not depend on
// the control-plane types. Until the Go control plane is deleted,
// internal/core declares type aliases to the types of this package.
package rtcore
