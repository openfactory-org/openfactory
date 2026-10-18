package definition

import (
	"strings"
	"testing"
)

func TestPlatformLinuxUser(t *testing.T) {
	machine := func(user string) map[string]string {
		return defTree("machines/r.yaml", "platform:\n  linux:\n    image: ghcr.io/acme/img:1\n"+user)
	}
	for _, c := range []struct{ yaml, want string }{
		{"", ""},
		{"    user: 2500\n", "2500"},
		{"    user: image\n", "image"},
		{"    user: null\n", ""},
	} {
		def := mustCompile(t, machine(c.yaml))
		if got := def.Machines["r"].User; got != c.want {
			t.Errorf("user %q: got %q, want %q", c.yaml, got, c.want)
		}
	}
	for _, c := range []struct{ yaml, want string }{
		{"    user: 0\n", "1 or more"},
		{"    user: -5\n", "1 or more"},
		{"    user: root\n", "numeric user ID"},
		{"    user: \"1000\"\n", "numeric user ID"},
		{"    user: 1.5\n", "numeric user ID"},
	} {
		_, errs := Compile(machine(c.yaml))
		found := false
		for _, e := range errs {
			if e.File == "machines/r.yaml" && e.Field == "platform.linux.user" && strings.Contains(e.Message, c.want) {
				found = true
			}
		}
		if !found {
			t.Errorf("user %q: errors = %+v, want one with %q", c.yaml, errs, c.want)
		}
	}
}
