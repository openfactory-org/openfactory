package k8stask

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// roleRule is one rule of the Role that the backend needs in the sandbox
// namespace.
type roleRule struct {
	group    string
	resource string
	verbs    []string
}

// requiredRules are the rules of the Role of the task backend. The
// NetworkPolicy rules are not here: a cluster without them still runs
// tasks that have no restriction (see checkNetworkPolicyAccess).
var requiredRules = []roleRule{
	{"batch", "jobs", []string{"create", "get", "list", "watch", "delete"}},
	{"", "pods", []string{"get", "list", "watch"}},
	{"", "pods/log", []string{"get"}},
	{"", "events", []string{"list"}},
}

// describe gives the rule as it appears in a Role, for example
// "jobs.batch (create, delete)".
func (r roleRule) describe(verbs []string) string {
	name := r.resource
	if r.group != "" {
		name += "." + r.group
	}
	return name + " (" + strings.Join(verbs, ", ") + ")"
}

// checkRoleAccess asks the API, with a SelfSubjectAccessReview for each verb,
// whether the account of the backend has the rules that it needs in the
// namespace. It returns an error that names each missing rule by resource and
// verb. If a review itself fails, the answer is unknown and the error is
// nil: the list calls of Preflight then give the answer.
func (b *Backend) checkRoleAccess(ctx context.Context) error {
	var missing []string
	for _, rule := range requiredRules {
		var denied []string
		for _, verb := range rule.verbs {
			body := map[string]any{
				"apiVersion": "authorization.k8s.io/v1",
				"kind":       "SelfSubjectAccessReview",
				"spec": map[string]any{"resourceAttributes": map[string]any{
					"namespace": b.ns, "verb": verb, "group": rule.group, "resource": rule.resource,
				}},
			}
			var out struct {
				Status struct {
					Allowed bool `json:"allowed"`
				} `json:"status"`
			}
			if err := b.do(ctx, http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews", "", body, &out); err != nil {
				b.log.Debug("preflight: access review failed", "resource", rule.resource, "verb", verb, "err", err)
				return nil
			}
			if !out.Status.Allowed {
				denied = append(denied, verb)
			}
		}
		if len(denied) > 0 {
			sort.Strings(denied)
			missing = append(missing, rule.describe(denied))
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("k8stask: preflight: the Role of the server lacks rules in namespace %q: %s: "+
		"add them to the Role of the sandbox namespace (see docs/self-hosting/kubernetes.md)",
		b.ns, strings.Join(missing, "; "))
}
