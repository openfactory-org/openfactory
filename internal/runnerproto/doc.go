// Package runnerproto holds the Go types of the runner protocol, version
// runner/v1. The runner is the process in the sandbox. It talks to the control
// plane of Open Factory with HTTP requests that carry JSON bodies.
//
// This package has only types, constants and small validation functions. It
// has no HTTP client and no server. A change to a JSON field name is a breaking change of the protocol.
package runnerproto
