package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

// openAPISpec is the hand-maintained OpenAPI 3.1 description of the REST API.
// TestOpenAPICoversRoutes keeps it in sync with the registered routes.
//
//go:embed openapi.json
var openAPISpec []byte

// OpenAPISpec returns the embedded OpenAPI document.
func OpenAPISpec() []byte { return openAPISpec }

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	spec := openAPISpec
	if s.opts.PublicURL != "" {
		var doc map[string]any
		if err := json.Unmarshal(openAPISpec, &doc); err == nil {
			doc["servers"] = []map[string]string{{"url": s.opts.PublicURL + BasePath}}
			if b, err := json.Marshal(doc); err == nil {
				spec = b
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(spec)
	return nil
}
