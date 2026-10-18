package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// markers returns a Markdown page with an empty generated section for each id.
func markers(ids ...string) string {
	var b strings.Builder
	b.WriteString("# Page\n\nHand-written text.\n\n")
	for _, id := range ids {
		b.WriteString("<!-- BEGIN GENERATED: " + id + " (make docs-gen) -->\n")
		b.WriteString("stale\n")
		b.WriteString("<!-- END GENERATED: " + id + " -->\n\n")
	}
	return b.String()
}

func allIDs() []string {
	ids := make([]string, 0, len(sections))
	for id := range sections {
		ids = append(ids, id)
	}
	return ids
}

func TestRenderFillsEverySection(t *testing.T) {
	schemas, err := loadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	out, err := render([]byte(markers(allIDs()...)), schemas)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "stale") {
		t.Fatal("a section kept its old content")
	}
	if !strings.Contains(s, "Hand-written text.") {
		t.Fatal("the text outside the markers changed")
	}
	for _, want := range []string{
		"| `schemaVersion` | string | yes |  | `v1` |",
		"| `platform.mac.version` | string |  | `\"26\"` |",
		"| `labels[].score` | number | yes |",
		"machines/&lt;name&gt;.yaml",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output has no %q", want)
		}
	}
	again, err := render(out, schemas)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != s {
		t.Fatal("a second render changed the output")
	}
}

func TestRenderErrors(t *testing.T) {
	schemas, err := loadSchemas()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, md, want string }{
		{"unknown section", markers(append(allIDs(), "nope")...), `unknown generated section "nope"`},
		{"missing section", markers("factory"), "no markers for the sections"},
		{"no end marker", "<!-- BEGIN GENERATED: factory (make docs-gen) -->\n", "no end marker"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := render([]byte(tc.md), schemas)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckFailsOnStaleFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "ref.md")
	if err := os.WriteFile(file, []byte(markers(allIDs()...)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run(file, true); err == nil || !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("check of a stale file: %v", err)
	}
	if err := run(file, false); err != nil {
		t.Fatal(err)
	}
	if err := run(file, true); err != nil {
		t.Fatalf("check after a write: %v", err)
	}
}
