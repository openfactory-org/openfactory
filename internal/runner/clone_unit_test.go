package runner

import (
	"strings"
	"testing"
)

func TestCloneArgs(t *testing.T) {
	got := strings.Join(cloneArgs("!helper", "https://h/r.git", "/w/r", "/cache/git/k.git"), " ")
	want := "-c credential.helper=!helper -c credential.useHttpPath=true -c safe.directory=/cache/git/k.git clone --reference /cache/git/k.git --dissociate -- https://h/r.git /w/r"
	if got != want {
		t.Errorf("args:\n got %s\nwant %s", got, want)
	}
	if got := strings.Join(cloneArgs("", "u", "d", ""), " "); got != "clone -- u d" {
		t.Errorf("plain clone: %s", got)
	}
}

func TestNoSymlinks(t *testing.T) {
	dir := t.TempDir()
	if err := noSymlinks(dir, "a/b/c"); err != nil {
		t.Fatal(err)
	}
}
