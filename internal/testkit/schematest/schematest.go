// Package schematest checks Go protocol types against the JSON Schemas in
// schemas/. A test fills a value of each type (Fill), marshals it and
// validates it against a definition of a schema file (Validator). The
// schemas set additionalProperties to false, so a Go field that the schema
// does not list fails the test, and so does a schema field with another type.
package schematest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Validator validates values against the schema files of one directory. It
// is safe for concurrent use: fake servers validate from their handlers.
type Validator struct {
	t   testing.TB
	dir string
	mu  sync.Mutex // guards c: the compiler is not safe for concurrent use
	c   *jsonschema.Compiler
}

func (v *Validator) compile(ref string) (*jsonschema.Schema, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.c.Compile(v.url(ref))
}

// New loads every *.json file of dir.
func New(t testing.TB, dir string) *Validator {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schema files in %s: %v", dir, err)
	}
	for _, f := range files {
		data, err := os.ReadFile(f) //nolint:gosec // G304: a test fixture path
		if err != nil {
			t.Fatal(err)
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if err := c.AddResource(URL(dir, filepath.Base(f)), doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return &Validator{t: t, dir: dir, c: c}
}

// URL is the URL under which a schema file is loaded.
func URL(dir, file string) string {
	abs, _ := filepath.Abs(filepath.Join(dir, file))
	return "file://" + filepath.ToSlash(abs)
}

// Validate marshals v and validates it against ref, a file name and a JSON
// pointer such as "session.json#/$defs/StartRequest".
func (v *Validator) Validate(ref string, value any) error {
	v.t.Helper()
	sch, err := v.compile(ref)
	if err != nil {
		v.t.Fatalf("compile %s: %v", ref, err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		v.t.Fatalf("marshal %T: %v", value, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		v.t.Fatal(err)
	}
	return sch.Validate(doc)
}

// ValidateJSON validates raw JSON against ref.
func (v *Validator) ValidateJSON(ref string, data []byte) error {
	v.t.Helper()
	sch, err := v.compile(ref)
	if err != nil {
		v.t.Fatalf("compile %s: %v", ref, err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		v.t.Fatal(err)
	}
	return sch.Validate(doc)
}

func (v *Validator) url(ref string) string {
	file, ptr, _ := strings.Cut(ref, "#")
	return URL(v.dir, file) + "#" + ptr
}

// MustValidate fails the test when value does not match ref.
func (v *Validator) MustValidate(ref string, value any) {
	v.t.Helper()
	if err := v.Validate(ref, value); err != nil {
		data, _ := json.Marshal(value)
		v.t.Errorf("%s does not match %T:\n%v\n%s", ref, value, err, data)
	}
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage{})
)

// Fill sets every field of the value that ptr points to: strings get the
// value of strs for their type (or "x"), numbers 1, booleans true, times a
// fixed time, slices and maps one element, pointers a new value. It returns
// ptr.
func Fill[T any](ptr *T, strs map[reflect.Type]string) *T {
	fill(reflect.ValueOf(ptr).Elem(), strs, 0)
	return ptr
}

func fill(v reflect.Value, strs map[reflect.Type]string, depth int) {
	if depth > 8 {
		return
	}
	t := v.Type()
	switch t {
	case timeType:
		v.Set(reflect.ValueOf(time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC)))
		return
	case rawType:
		v.Set(reflect.ValueOf(json.RawMessage(`{"k":1}`)))
		return
	}
	switch t.Kind() {
	case reflect.String:
		s, ok := strs[t]
		if !ok {
			s = "x"
		}
		v.SetString(s)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1.5)
	case reflect.Pointer:
		p := reflect.New(t.Elem())
		fill(p.Elem(), strs, depth+1)
		v.Set(p)
	case reflect.Slice:
		s := reflect.MakeSlice(t, 1, 1)
		fill(s.Index(0), strs, depth+1)
		v.Set(s)
	case reflect.Map:
		m := reflect.MakeMap(t)
		k := reflect.New(t.Key()).Elem()
		fill(k, strs, depth+1)
		e := reflect.New(t.Elem()).Elem()
		fill(e, strs, depth+1)
		m.SetMapIndex(k, e)
		v.Set(m)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if t.Field(i).IsExported() {
				fill(v.Field(i), strs, depth+1)
			}
		}
	case reflect.Interface:
		v.Set(reflect.ValueOf("x"))
	}
}
