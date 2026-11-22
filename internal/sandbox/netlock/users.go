package netlock

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Users makes sure that the agent and proxy users exist.
type Users struct {
	Exec Executor
	// Lookup returns the uid of a user name, and false when the user does
	// not exist. Default: os/user.
	Lookup func(name string) (uid int, ok bool, err error)
	// SudoersDir is the root of the sudoers files to check. Default "/etc".
	SudoersDir string
}

func lookupUser(name string) (int, bool, error) {
	u, err := user.Lookup(name)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return 0, false, nil
		}
		return 0, false, err
	}
	n, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, false, fmt.Errorf("user %s has uid %q", name, u.Uid)
	}
	return n, true, nil
}

func (u Users) lookup(name string) (int, bool, error) {
	if u.Lookup != nil {
		return u.Lookup(name)
	}
	return lookupUser(name)
}

// Ensure creates the agent user (agentUID, home AgentHome) and the proxy user
// (proxyUID, no home, no shell) when they are missing. An existing user with
// another uid is an error. It also checks that the agent has no sudo rule.
func (u Users) Ensure(ctx context.Context, agentUID, proxyUID int) error {
	if err := u.ensure(ctx, AgentUser, agentUID, []string{"-m", "-d", AgentHome, "-s", "/bin/bash"}); err != nil {
		return err
	}
	if err := u.ensure(ctx, ProxyUser, proxyUID, []string{"-r", "-M", "-d", "/nonexistent", "-s", "/usr/sbin/nologin"}); err != nil {
		return err
	}
	return u.checkSudo()
}

func (u Users) ensure(ctx context.Context, name string, uid int, opts []string) error {
	got, ok, err := u.lookup(name)
	if err != nil {
		return fmt.Errorf("netlock: look up user %s: %w", name, err)
	}
	if ok {
		if got != uid {
			return fmt.Errorf("netlock: user %s has uid %d, want %d", name, got, uid)
		}
		return nil
	}
	id := strconv.Itoa(uid)
	// The group may exist already (for example from the image).
	if _, err := u.Exec.Run(ctx, "groupadd", "-g", id, name); err != nil {
		if _, err2 := u.Exec.Run(ctx, "getent", "group", id); err2 != nil {
			return fmt.Errorf("netlock: create group %s: %w", name, err)
		}
	}
	args := append([]string{"-u", id, "-g", id}, opts...)
	args = append(args, name)
	if _, err := u.Exec.Run(ctx, "useradd", args...); err != nil {
		return fmt.Errorf("netlock: create user %s: %w", name, err)
	}
	return nil
}

// checkSudo fails when a sudoers file gives the agent user or its group a
// rule: the agent could then remove the lock.
func (u Users) checkSudo() error {
	root := u.SudoersDir
	if root == "" {
		root = "/etc"
	}
	files := []string{filepath.Join(root, "sudoers")}
	if entries, err := os.ReadDir(filepath.Join(root, "sudoers.d")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				files = append(files, filepath.Join(root, "sudoers.d", e.Name()))
			}
		}
	}
	for _, f := range files {
		found, err := grantsUser(f, AgentUser)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("netlock: read %s: %w", f, err)
		}
		if found {
			return fmt.Errorf("netlock: %s gives the %s user sudo rights; remove the rule from the image", f, AgentUser)
		}
	}
	return nil
}

func grantsUser(path, name string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a sudoers path, not user input
	if err != nil {
		return false, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		first := strings.Fields(line)[0]
		if first == name || first == "%"+name {
			return true, nil
		}
	}
	return false, sc.Err()
}

// ChownTree gives a directory tree to uid:gid. It does not follow symbolic
// links, and it stays inside dir (os.Root), so a link that the agent plants
// cannot redirect it.
func ChownTree(dir string, uid, gid int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return root.Lchown(p, uid, gid)
	})
}
