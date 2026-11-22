package gitcachekey

import "testing"

func TestKeyNormalizes(t *testing.T) {
	a := Key("https://GitHub.com/acme/app.git")
	for _, u := range []string{
		"https://github.com/acme/app",
		"https://user:pw@github.com/acme/app/",
		"HTTPS://github.com/acme/app.git",
		" https://github.com/acme/app.git?x=1#frag ",
	} {
		if got := Key(u); got != a {
			t.Errorf("Key(%q) = %q, want %q", u, got, a)
		}
	}
	if !Valid(a) {
		t.Errorf("key %q is not 16 hex digits", a)
	}
}

func TestKeyDiffersAndRejects(t *testing.T) {
	tests := []struct {
		in    string
		empty bool
	}{
		{"https://github.com/acme/other", false},
		{"https://github.com/Acme/app", false}, // the path keeps its case
		{"not a url", true},
		{"", true},
		{"/local/path", true},
	}
	a := Key("https://github.com/acme/app")
	for _, tt := range tests {
		got := Key(tt.in)
		if tt.empty != (got == "") {
			t.Errorf("Key(%q) = %q, want empty=%v", tt.in, got, tt.empty)
		}
		if got != "" && got == a {
			t.Errorf("Key(%q) equals the key of another repository", tt.in)
		}
	}
}

func TestValid(t *testing.T) {
	for k, want := range map[string]bool{"0123456789abcdef": true, "0123456789ABCDEF": false, "abc": false, "0123456789abcdeg": false} {
		if Valid(k) != want {
			t.Errorf("Valid(%q) = %v", k, !want)
		}
	}
}
