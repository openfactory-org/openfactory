package guest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

func TestBuildEnv(t *testing.T) {
	p := mmds.Payload{
		Image:   &mmds.Image{Env: []string{"PATH=/opt/bin:/usr/bin", "LANG=C.UTF-8", "HOME=/wrong"}},
		Env:     map[string]string{"OF_RUN_ID": "run_a", "OF_BOOTSTRAP_TOKEN": "ofb_x", "LANG": "en_US.UTF-8"},
		Network: &mmds.Network{ProxyURL: "http://10.213.0.1:3128", NoProxy: "localhost,127.0.0.1", EgressReportURL: "http://10.213.0.1:3129/blocked"},
		Disks:   &mmds.Disks{Cache: []mmds.CacheDisk{{Drive: "cache-0", Key: "0123456789abcdef"}}},
	}
	env := strings.Join(BuildEnv(p, "/home/agent"), "\n")
	for _, want := range []string{
		"PATH=/opt/bin:/usr/bin", "HOME=/home/agent", "USER=agent", "OF_WORKSPACE=/workspace",
		"OF_RUNNER_STATE_DIR=/var/lib/openfactory-runner", "HTTPS_PROXY=http://10.213.0.1:3128", "no_proxy=localhost,127.0.0.1",
		"OF_NETGUARD_REPORT=http://10.213.0.1:3129/blocked", "OF_GIT_CACHE_DIR=/cache/git", "OF_RUN_ID=run_a", "LANG=en_US.UTF-8",
	} {
		if !strings.Contains(env+"\n", want+"\n") {
			t.Errorf("env lacks %s\n%s", want, env)
		}
	}
	if strings.Count(env, "LANG=") != 1 || strings.Contains(env, "HOME=/wrong") {
		t.Errorf("duplicates:\n%s", env)
	}
	// Unrestricted: no proxy variables.
	env = strings.Join(BuildEnv(mmds.Payload{Network: &mmds.Network{DNS: []string{"10.213.0.1"}}}, "/root"), "\n")
	if strings.Contains(env, "PROXY") || !strings.Contains(env, "PATH=/usr/local/sbin") {
		t.Errorf("unrestricted env:\n%s", env)
	}
}

func TestMapDisks(t *testing.T) {
	root := t.TempDir()
	for dev, serial := range map[string]string{"vda": "tools", "vdb": "scratch", "vdc": "image", "vdd": "cache-0"} {
		_ = os.MkdirAll(filepath.Join(root, dev), 0o755)
		_ = os.WriteFile(filepath.Join(root, dev, "serial"), []byte(serial+"\n"), 0o444)
	}
	got := MapDisks(root, []string{"image", "scratch", "cache-0"})
	if got["image"] != "/dev/vdc" || got["scratch"] != "/dev/vdb" || got["cache-0"] != "/dev/vdd" {
		t.Fatalf("by serial %v", got)
	}
	root2 := t.TempDir()
	for _, dev := range []string{"vda", "vdb", "vdc"} {
		_ = os.MkdirAll(filepath.Join(root2, dev), 0o755)
	}
	got = MapDisks(root2, []string{"image", "scratch"})
	if got["tools"] != "/dev/vda" || got["image"] != "/dev/vdb" || got["scratch"] != "/dev/vdc" {
		t.Fatalf("by order %v", got)
	}
}

func TestFilesAndUsers(t *testing.T) {
	if Hosts("of-1") != "127.0.0.1 localhost of-1\n" || ResolvConf(nil) != "" || ResolvConf([]string{"10.0.0.1"}) != "nameserver 10.0.0.1\n" {
		t.Fatal("files")
	}
	passwd := "root:x:0:0:root:/root:/bin/sh\nnode:x:1000:1000::/home/node:/bin/sh\n"
	if uid, gid, home := ResolveUser(&mmds.User{UID: 1001, GID: 1001, Name: "node"}, passwd); uid != 1000 || gid != 1000 || home != "/home/node" {
		t.Fatalf("named user %d %d %s", uid, gid, home)
	}
	if uid, _, home := ResolveUser(&mmds.User{UID: 1001, GID: 1001, Home: "/home/agent", Name: "missing"}, passwd); uid != 1001 || home != "/home/agent" {
		t.Fatal("fallback user")
	}
	if OOMKills("pgfault 1\noom_kill 3\n") != 3 {
		t.Fatal("oom kills")
	}
}
