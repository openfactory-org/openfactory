package runner

import (
	"encoding/base64"
	"net/url"
	"strings"
	"testing"
)

// TestRedactRawBase64AndURLEncodedForms checks that an agent that prints a
// secret, its base64 form and its URL-encoded form gets *** three times.
func TestRedactRawBase64AndURLEncodedForms(t *testing.T) {
	secret := "pa ss/word+1234&x=y"
	text := `{"out":"` + secret + ` | ` + base64.StdEncoding.EncodeToString([]byte(secret)) + ` | ` + url.QueryEscape(secret) + `"}`
	got := string(redactJSON([]byte(text), []string{secret}))
	if n := strings.Count(got, "***"); n != 3 {
		t.Fatalf("got %d marks in %s", n, got)
	}
	if strings.Contains(got, "word") {
		t.Fatalf("a form of the secret stays: %s", got)
	}
}
