// Package kubernetes holds what the task backend of Kubernetes (k8stask)
// needs: the cluster configuration and client credentials, and small helpers
// for pod templates and labels. It talks to the Kubernetes API directly over
// HTTPS (no client-go).
package kubernetes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/openfactory-org/openfactory/internal/core"
	"github.com/openfactory-org/openfactory/internal/sandbox"
)

// DefaultImage is used when neither environment nor machine sets an image
// and the setting OF_SANDBOX_IMAGE is empty. It is a variable because a
// release build sets sandbox.DefaultImage with a linker flag.
var DefaultImage = sandbox.DefaultImage

// TaskContainer is the name of the main container of a task Pod.
const TaskContainer = "sandbox"

// Authorize adds the credentials of the config to the headers of a request.
// Other backends that call the API over HTTPS use it.
func (c *RestConfig) Authorize(h http.Header) error { return c.authorize(h) }

// ParsePodTemplate parses a raw PodSpec in YAML or JSON. It accepts a Pod or a
// pod template wrapper and returns the spec. An empty text gives an empty map.
func ParsePodTemplate(raw string) (map[string]any, error) {
	tmpl := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return tmpl, nil
	}
	if err := yaml.Unmarshal([]byte(raw), &tmpl); err != nil {
		return nil, fmt.Errorf("%w: pod_template: %w", core.ErrInvalid, err)
	}
	if spec, ok := tmpl["spec"].(map[string]any); ok && tmpl["containers"] == nil {
		tmpl = spec
	}
	if _, err := json.Marshal(tmpl); err != nil {
		return nil, fmt.Errorf("%w: pod_template: %w", core.ErrInvalid, err)
	}
	return tmpl, nil
}

// DeepCopy returns a deep copy of a value that holds maps and slices of the
// kind that a YAML or JSON decoder makes.
func DeepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = DeepCopy(vv)
		}
		return m
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = DeepCopy(vv)
		}
		return s
	default:
		return v
	}
}

var labelSanitizer = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// LabelValue makes s a valid label value (63 characters at most, with
// alphanumeric ends).
func LabelValue(s string) string {
	v := labelSanitizer.ReplaceAllString(s, "-")
	if len(v) > 63 {
		v = v[:63]
	}
	return strings.Trim(v, "-_.")
}
