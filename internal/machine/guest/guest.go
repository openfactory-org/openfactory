// Package guest is vm-init, PID 1 of a worker microVM. It mounts the disks,
// reads the session document from MMDS once (the host then clears it),
// configures the network, starts the runner as an unprivileged child,
// reports its exit over the vsock control stream and reboots, which ends
// Firecracker. The PID 1 code is Linux only (init_linux.go); this file has
// the parts that tests run anywhere.
package guest

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/openfactory-org/openfactory/internal/machine/mmds"
)

// Version of vm-init in its hello.
const Version = "1"

// Paths inside the guest after pivot_root.
const (
	Workspace     = "/workspace"
	RunnerState   = "/var/lib/openfactory-runner"
	ToolsMount    = "/.openfactory"
	RunnerBinary  = "/.openfactory/openfactory"
	GitCacheMount = "/cache/git"
)

// BuildEnv returns the environment of the runner, later entries winning:
// the image env, HOME/USER/LOGNAME/SHELL, the workspace variables, the
// proxy variables of a restricted VM, OF_GIT_CACHE_DIR, then the task env.
func BuildEnv(p mmds.Payload, home string) []string {
	env := map[string]string{}
	var order []string
	set := func(k, v string) {
		if _, ok := env[k]; !ok {
			order = append(order, k)
		}
		env[k] = v
	}
	if p.Image != nil {
		for _, kv := range p.Image.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				set(k, v)
			}
		}
	}
	if _, ok := env["PATH"]; !ok {
		set("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	set("HOME", home)
	for k, v := range map[string]string{"USER": "agent", "LOGNAME": "agent", "SHELL": "/bin/sh"} {
		if _, ok := env[k]; !ok {
			set(k, v)
		}
	}
	set("OF_WORKSPACE", Workspace)
	set("OF_RUNNER_STATE_DIR", RunnerState)
	if n := p.Network; n != nil && n.ProxyURL != "" {
		for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy"} {
			set(k, n.ProxyURL)
		}
		set("NO_PROXY", n.NoProxy)
		set("no_proxy", n.NoProxy)
		if n.EgressReportURL != "" {
			set("OF_NETGUARD_REPORT", n.EgressReportURL)
		}
	}
	if p.Disks != nil && len(p.Disks.Cache) > 0 {
		set("OF_GIT_CACHE_DIR", GitCacheMount)
	}
	keys := make([]string, 0, len(p.Env))
	for k := range p.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		set(k, p.Env[k])
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+env[k])
	}
	return out
}

// MapDisks finds the block device of each drive ID: Firecracker sets the
// virtio-blk serial to the drive ID (/sys/block/vdX/serial). Devices without
// a serial are matched in order: tools (vda), image, scratch, cache-0...
func MapDisks(sysBlock string, drives []string) map[string]string {
	out := map[string]string{}
	matches, _ := filepath.Glob(filepath.Join(sysBlock, "vd*"))
	sort.Strings(matches)
	var unnamed []string
	for _, m := range matches {
		dev := "/dev/" + filepath.Base(m)
		serial, _ := os.ReadFile(filepath.Join(m, "serial"))
		s := strings.TrimSpace(string(serial))
		if s == "" {
			unnamed = append(unnamed, dev)
			continue
		}
		out[s] = dev
	}
	if len(out) == 0 && len(unnamed) > 0 {
		order := append([]string{"tools"}, drives...)
		for i, d := range order {
			if i < len(unnamed) {
				out[d] = unnamed[i]
			}
		}
	}
	return out
}

// Hosts returns /etc/hosts.
func Hosts(hostname string) string {
	return "127.0.0.1 localhost " + hostname + "\n"
}

// ResolvConf returns /etc/resolv.conf: one nameserver per entry, empty in
// restricted mode.
func ResolvConf(dns []string) string {
	var b strings.Builder
	for _, d := range dns {
		b.WriteString("nameserver " + d + "\n")
	}
	return b.String()
}

// LookupUser finds a user name in an /etc/passwd file.
func LookupUser(passwd, name string) (uid, gid int, home string, ok bool) {
	sc := bufio.NewScanner(strings.NewReader(passwd))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) >= 6 && f[0] == name {
			u, err1 := strconv.Atoi(f[2])
			g, err2 := strconv.Atoi(f[3])
			if err1 == nil && err2 == nil {
				return u, g, f[5], true
			}
		}
	}
	return 0, 0, "", false
}

// OOMKills returns the oom_kill counter of /proc/vmstat.
func OOMKills(vmstat string) int {
	for _, line := range strings.Split(vmstat, "\n") {
		if v, ok := strings.CutPrefix(line, "oom_kill "); ok {
			n, _ := strconv.Atoi(strings.TrimSpace(v))
			return n
		}
	}
	return 0
}

// ResolveUser applies a user name of the document against the image's
// passwd. It returns the uid, gid and home to use.
func ResolveUser(u *mmds.User, passwd string) (int, int, string) {
	if u == nil {
		return 1001, 1001, "/home/agent"
	}
	if u.Name != "" {
		if uid, gid, home, ok := LookupUser(passwd, u.Name); ok {
			if home == "" {
				home = "/home/" + u.Name
			}
			return uid, gid, home
		}
	}
	home := u.Home
	if home == "" {
		home = "/home/agent"
	}
	return u.UID, u.GID, home
}
