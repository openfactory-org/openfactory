// Package direct implements remote workers: processes that connect to the
// control plane over an outbound WebSocket, so no inbound firewall port is
// needed on the worker host. A worker runs the tasks that the control plane
// assigns, and the runner in each task talks to the control plane directly.
//
// The hub is part of the control plane (WorkerHubDO in apps/control-plane).
// It accepts worker connections at GET /api/v1/workers/connect. This package
// is the worker side: RunWorkerV2 connects (reconnecting with exponential
// backoff), registers its id/name/labels/capacity/backend and runs tasks with
// a local rtcore.TaskBackend (the process backend of the dev worker, or the
// microVM backend).
//
// There is one wire protocol, version 2 (WebSocket subprotocol
// "openfactory.worker.v2"). protocolv2.go describes it. Version 1, which
// sent sandbox operations (exec, read and write file) to the worker, was
// removed. A worker that offers only version 1 is refused.
package direct

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/openfactory-org/openfactory/internal/rtcore"
)

const (
	// ConnectPath is the worker connection endpoint.
	ConnectPath = "/api/v1/workers/connect"
	// HeaderWorkerID is the upgrade header with the worker ID. The control
	// plane routes the connection by it; hello and auth.response must name
	// the same ID.
	HeaderWorkerID = "OF-Worker-ID"
	// closePolicy is the WebSocket close code for a refused connection.
	closePolicy = 1008
	// pingFrame is the liveness frame of an enrolled worker. The hub answers
	// {"type":"pong"}.
	pingFrame = `{"type":"ping"}`
)

// Message types.
const (
	typeHello     = "hello"
	typeWelcome   = "welcome"
	typeHeartbeat = "heartbeat"
	typeRequest   = "request"
	typeResponse  = "response"
	typeEvent     = "event"

	typeAuthChallenge = "auth.challenge"
	typeAuthResponse  = "auth.response"
)

// Error codes.
const (
	codeNotFound   = "not_found"
	codeAtCapacity = "at_capacity"
	codeInternal   = "internal"
)

type message struct {
	Type   string          `json:"type"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// Welcome is the hub's reply to HelloV2.
type Welcome struct {
	ConnectionID string `json:"connection_id"`
	HeartbeatMS  int64  `json:"heartbeat_ms"`
	// AccessToken is a short-lived token of an enrolled worker (prefix
	// ofw_) for its REST calls, for example the git credential of the
	// mirror cache. It is kept in memory only.
	AccessToken          string     `json:"access_token,omitempty"` //nolint:gosec // G117: the token goes to the worker on purpose
	AccessTokenExpiresAt *time.Time `json:"access_token_expires_at,omitempty"`
}

// Heartbeat reports worker load.
type Heartbeat struct {
	Active   int `json:"active"`
	Capacity int `json:"capacity"`
}

var workerIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ReservedWorkerIDs cannot be used by remote workers: they name the team
// default (server), built-in hosts and the sandbox providers, which are also
// workerHost values.
var ReservedWorkerIDs = []string{"server", "local", "direct", "managed", "cloudflare-sandbox", "daytona", "boatdev"}

// ValidateWorkerID checks a worker id: 1–64 characters of letters, digits,
// ".", "_" and "-", starting with a letter or digit; ids that name built-in
// worker hosts or start with "openfactory" are reserved.
func ValidateWorkerID(id string) error {
	if !workerIDRe.MatchString(id) {
		return fmt.Errorf("%w: worker id %q must be 1-64 characters of letters, digits, '.', '_' or '-'", rtcore.ErrInvalid, id)
	}
	lower := strings.ToLower(id)
	if strings.HasPrefix(lower, "openfactory") {
		return fmt.Errorf("%w: worker ids starting with \"openfactory\" are reserved", rtcore.ErrInvalid)
	}
	for _, r := range ReservedWorkerIDs {
		if lower == r {
			return fmt.Errorf("%w: worker id %q is reserved", rtcore.ErrInvalid, id)
		}
	}
	return nil
}
