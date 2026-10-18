// Package mergeguard enforces the rule that agents open pull requests and
// people merge them. It reads the MCP requests that a run sends through the
// control plane (the MCP proxy and the built-in integration servers) and
// refuses each tools/call of a merge tool, unless the factory sets
// allowAgentMerge.
//
// The guard is a control on the MCP path only. It does not see what an agent
// does with a forge credential outside MCP. See docs/security.md, section
// "Agents never merge", for the limits.
package mergeguard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode"
)

// maxBody is the largest MCP request body that the guard reads.
const maxBody = 16 << 20

// RefusalText is the text of the tool result that an agent gets for a
// refused merge.
const RefusalText = "Refused: agents in this factory cannot merge pull requests or merge requests. " +
	"Open the pull request and hand it off; a person merges it. " +
	"To allow merges by agents, set allowAgentMerge: true in factory.yaml."

// Policy decides about merge calls of one request.
type Policy struct {
	// Allow reports whether the run of the request can merge. Nil means never.
	Allow func(r *http.Request) bool
	// Record is called for each merge call, with the decision. It can be nil.
	Record func(r *http.Request, tool string, allowed bool)
}

// rpcRequest is one JSON-RPC request of MCP.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  struct {
		Name string `json:"name"`
	} `json:"params"`
}

// Wrap returns a handler that refuses merge tool calls and passes all other
// requests to next unchanged.
func Wrap(next http.Handler, p Policy) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Body == nil {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		r.Body.Close()
		if err != nil {
			http.Error(w, "could not read the request", http.StatusBadRequest)
			return
		}
		if len(body) > maxBody {
			http.Error(w, "the MCP request is too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))

		calls, batch := parse(body)
		var merges []rpcRequest
		for _, c := range calls {
			if c.Method == "tools/call" && IsMergeTool(c.Params.Name) {
				merges = append(merges, c)
			}
		}
		if len(merges) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		allowed := p.Allow != nil && p.Allow(r)
		for _, c := range merges {
			if p.Record != nil {
				p.Record(r, c.Params.Name, allowed)
			}
		}
		if allowed {
			next.ServeHTTP(w, r)
			return
		}
		refuse(w, calls, batch)
	})
}

// parse reads one JSON-RPC message or a batch. A body that is not JSON-RPC
// gives no calls.
func parse(body []byte) (calls []rpcRequest, batch bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, false
	}
	if trimmed[0] == '[' {
		var list []rpcRequest
		if json.Unmarshal(trimmed, &list) != nil {
			return nil, true
		}
		return list, true
	}
	var one rpcRequest
	if json.Unmarshal(trimmed, &one) != nil {
		return nil, false
	}
	return []rpcRequest{one}, false
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  *toolResult     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// refuse answers the request without calling the server. A merge call gets a
// tool result with isError, so that the agent reads the reason. In a batch,
// the other requests get a JSON-RPC error: the guard does not split a batch.
func refuse(w http.ResponseWriter, calls []rpcRequest, batch bool) {
	out := make([]rpcResponse, 0, len(calls))
	for _, c := range calls {
		if len(c.ID) == 0 {
			continue // a notification has no response
		}
		resp := rpcResponse{JSONRPC: "2.0", ID: c.ID}
		if c.Method == "tools/call" && IsMergeTool(c.Params.Name) {
			resp.Result = &toolResult{Content: []textContent{{Type: "text", Text: RefusalText}}, IsError: true}
		} else {
			resp.Error = &rpcError{Code: -32600, Message: "this batch has a merge call that the platform refused; send the other requests again without it"}
		}
		out = append(out, resp)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-OF-Merge-Refused", "true")
	if len(out) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.WriteHeader(http.StatusOK)
	if batch {
		_ = json.NewEncoder(w).Encode(out)
		return
	}
	_ = json.NewEncoder(w).Encode(out[0])
}

// IsMergeTool reports whether an MCP tool name names a merge of a pull
// request or a merge request, or a change that makes the forge merge later
// (auto-merge, a merge queue). Names are compared as words, so that
// merge_pull_request, mergePullRequest, forge.merge-pr and
// accept_merge_request all match, and get_merge_request does not.
func IsMergeTool(name string) bool {
	toks := words(name)
	n := len(toks)
	if n == 0 {
		return false
	}
	is := func(i int, set ...string) bool {
		if i < 0 || i >= n {
			return false
		}
		for _, s := range set {
			if toks[i] == s {
				return true
			}
		}
		return false
	}
	target := func(i int) bool {
		// pull_request, pullrequest, pr, prs, mr, merge_request, branch, change.
		return is(i, "pr", "prs", "mr", "mrs", "pullrequest", "pullrequests", "branch", "branches", "into", "changes", "change") ||
			(is(i, "pull") && is(i+1, "request", "requests")) ||
			(is(i, "merge") && is(i+1, "request", "requests"))
	}
	for i := 0; i < n; i++ {
		switch toks[i] {
		case "automerge":
			return true
		case "auto":
			if is(i+1, "merge") {
				return true
			}
		case "enqueue":
			if target(i+1) || is(i+1, "merge") {
				return true
			}
		case "merge":
			switch {
			case is(i+1, "request", "requests"):
				// The noun "merge request". merge_merge_request matched at
				// the token before.
			case is(i+1, "queue"):
				// Adding to a merge queue merges later.
				if is(i-1, "add", "enqueue") || (is(i-1, "to") && is(i-2, "add")) || is(i+2, "add", "enqueue") {
					return true
				}
			case i == n-1:
				// merge, pr_merge, pull_request_merge, squash_merge.
				return true
			case target(i + 1):
				return true
			case is(i+1, "and", "squash", "rebase") || is(i-1, "squash", "rebase", "approve", "and"):
				return true
			}
		case "accept", "complete":
			if target(i + 1) {
				return true
			}
		}
	}
	return false
}

// words splits a tool name into lower-case words at non-letters, non-digits
// and lower-to-upper case changes.
func words(name string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	rs := []rune(name)
	for i, r := range rs {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1])) {
				flush()
			}
			cur.WriteRune(unicode.ToLower(r))
		default:
			flush()
		}
	}
	flush()
	return out
}
