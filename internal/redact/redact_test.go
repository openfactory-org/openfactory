package redact

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

const secret = "s3cr3t/value+with&chars=1"

func TestForms(t *testing.T) {
	r := New(secret)
	for name, form := range map[string]string{
		"raw":           secret,
		"base64":        base64.StdEncoding.EncodeToString([]byte(secret)),
		"base64 raw":    base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"base64 url":    base64.URLEncoding.EncodeToString([]byte(secret)),
		"url query":     url.QueryEscape(secret),
		"url path":      url.PathEscape(secret),
		"json escaped":  `s3cr3t\/value`, // not a form of this secret: stays
		"unrelated":     "hello",
		"empty secrets": "",
	} {
		got := r.String("a " + form + " b")
		switch name {
		case "json escaped", "unrelated", "empty secrets":
			if got != "a "+form+" b" {
				t.Errorf("%s: changed to %q", name, got)
			}
		default:
			if got != "a *** b" {
				t.Errorf("%s: got %q", name, got)
			}
		}
	}
}

func TestThreeFormsInOneText(t *testing.T) {
	r := New("token-abc123XYZ")
	text := "raw token-abc123XYZ b64 " + base64.StdEncoding.EncodeToString([]byte("token-abc123XYZ")) + " url " + url.QueryEscape("token-abc123XYZ")
	// The URL-encoded form of this value has no change, so it equals the raw form.
	got := r.String(text)
	if strings.Count(got, Mark) != 3 || strings.Contains(got, "abc123") {
		t.Errorf("got %q", got)
	}
}

func TestShortValuesAreIgnored(t *testing.T) {
	r := New("short", "1234567", "12345678")
	if r.Len() != 1 {
		t.Errorf("Len = %d", r.Len())
	}
	if got := r.String("short 1234567 12345678"); got != "short 1234567 ***" {
		t.Errorf("got %q", got)
	}
	// A lower minimum keeps short raw values but gives no encoded forms.
	r2 := NewWithMin(1, "s3cret")
	if got := r2.String("s3cret " + base64.StdEncoding.EncodeToString([]byte("s3cret"))); got != "*** "+base64.StdEncoding.EncodeToString([]byte("s3cret")) {
		t.Errorf("got %q", got)
	}
}

func TestLongestFirst(t *testing.T) {
	r := New("password-123", "password-123-and-more")
	if got := r.String("x password-123-and-more y"); got != "x *** y" {
		t.Errorf("got %q", got)
	}
}

func TestJSON(t *testing.T) {
	r := New(`quote"and\back-slash-1`)
	raw, _ := json.Marshal(map[string]string{"k": `quote"and\back-slash-1`})
	out := r.JSON(raw)
	if !json.Valid(out) || strings.Contains(string(out), "back-slash") {
		t.Errorf("out = %s", out)
	}
	// A result that is not valid JSON is replaced.
	r2 := New(`"abcdefgh`)
	if got := string(r2.JSON([]byte(`{"a":"x"}"abcdefgh`))); got != `{"redacted":true}` {
		t.Errorf("got %s", got)
	}
	if got := string((*Redactor)(nil).JSON([]byte(`{}`))); got != `{}` {
		t.Errorf("nil redactor: %s", got)
	}
}

func TestData(t *testing.T) {
	r := New("super-secret-1")
	m := map[string]any{"cmd": "echo super-secret-1", "n": 3, "nested": map[string]any{"list": []any{"a super-secret-1 b", 2}}}
	got := r.Data(m)
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "super-secret") || got["n"] != float64(3) {
		t.Errorf("got %s", b)
	}
	// The input is not changed.
	if m["cmd"] != "echo super-secret-1" {
		t.Error("the input changed")
	}
	if len(New().Data(m)) != 3 {
		t.Error("an empty redactor must return the map")
	}
}

func TestAddAndNil(t *testing.T) {
	var nilR *Redactor
	nilR.Add("abcdefgh")
	if nilR.String("abcdefgh") != "abcdefgh" || nilR.Len() != 0 {
		t.Error("nil redactor")
	}
	r := New()
	if r.String("later-secret") != "later-secret" {
		t.Error("nothing to redact yet")
	}
	r.Add("later-secret")
	if r.String("later-secret") != Mark {
		t.Error("Add has no effect")
	}
}
