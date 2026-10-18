// Command openapi-baseline writes the committed OpenAPI baseline that CI diffs
// against to detect breaking REST API changes.
//
//	go run ./scripts/openapi-baseline            # write api/openapi.v1.baseline.json
//	go run ./scripts/openapi-baseline -check     # fail when the baseline is missing or invalid
//
// The source is internal/api/openapi.json, the file the server embeds
// (internal/api.OpenAPISpec). Regenerate the baseline only when a release
// ships: the baseline is the contract of the last released v1 API.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	spec := flag.String("spec", "internal/api/openapi.json", "OpenAPI document embedded by the server")
	out := flag.String("out", "api/openapi.v1.baseline.json", "baseline file to write")
	check := flag.Bool("check", false, "only verify that the baseline exists and is a v1 OpenAPI document")
	flag.Parse()
	if err := run(*spec, *out, *check); err != nil {
		fmt.Fprintln(os.Stderr, "openapi-baseline:", err)
		os.Exit(1)
	}
}

func run(spec, out string, check bool) error {
	if check {
		b, err := os.ReadFile(out)
		if err != nil {
			return fmt.Errorf("%w (generate it with `make openapi-baseline`)", err)
		}
		_, err = canonical(b)
		return err
	}
	b, err := os.ReadFile(spec)
	if err != nil {
		return err
	}
	c, err := canonical(b)
	if err != nil {
		return fmt.Errorf("%s: %w", spec, err)
	}
	if err := os.WriteFile(out, c, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s from %s\n", out, spec)
	return nil
}

// canonical validates an OpenAPI 3 document with a 1.x info.version and
// returns it re-encoded with sorted keys and two-space indentation, so
// baseline diffs in review stay readable.
func canonical(b []byte) ([]byte, error) {
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("not JSON: %w", err)
	}
	if v, _ := doc["openapi"].(string); !strings.HasPrefix(v, "3.") {
		return nil, errors.New(`"openapi" must be a 3.x version`)
	}
	info, _ := doc["info"].(map[string]any)
	if v, _ := info["version"].(string); !strings.HasPrefix(v, "1.") {
		return nil, fmt.Errorf("info.version %q: the v1 baseline needs a 1.x API version", v)
	}
	if paths, _ := doc["paths"].(map[string]any); len(paths) == 0 {
		return nil, errors.New("document has no paths")
	}
	delete(doc, "servers") // deployment-specific
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
