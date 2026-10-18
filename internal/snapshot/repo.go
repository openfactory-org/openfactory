package snapshot

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CaptureOptions are the inputs of CaptureRepo.
type CaptureOptions struct {
	RepoDir string // absolute path of the git working directory
	Path    string // the path of the repository in the workspace, stored in the entry
	BaseSHA string // the commit of the clone; used when the branch has no upstream
	OutDir  string // the directory of the snapshot part; files go under OutDir/repos
	Index   int    // the number of the repository; file names are repos/<Index>.bundle and so on
}

// CaptureRepo saves the state of one git working directory under
// o.OutDir/repos and returns the manifest entry and the list of things that
// the snapshot does not contain.
func CaptureRepo(ctx context.Context, o CaptureOptions) (RepoEntry, []Skipped, error) {
	e := RepoEntry{Path: o.Path}
	var skipped []Skipped

	head, err := git(ctx, o.RepoDir, "rev-parse", "HEAD")
	if err != nil {
		return e, nil, fmt.Errorf("snapshot: read HEAD: %w", err)
	}
	e.HeadSHA = head

	// A detached HEAD makes this command fail; the branch is then empty.
	if b, err := git(ctx, o.RepoDir, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		e.Branch = b
	}
	if r, err := git(ctx, o.RepoDir, "remote", "get-url", "origin"); err == nil {
		e.Remote = r
	}

	if up, err := git(ctx, o.RepoDir, "merge-base", "HEAD", "@{upstream}"); err == nil && up != "" {
		e.UpstreamSHA = up
	} else if o.BaseSHA != "" {
		e.UpstreamSHA = o.BaseSHA
	} else {
		return e, nil, errors.New("snapshot: the branch has no upstream and no base commit is known")
	}

	reposDir := filepath.Join(o.OutDir, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return e, nil, fmt.Errorf("snapshot: create repos directory: %w", err)
	}
	idx := strconv.Itoa(o.Index)

	if e.UpstreamSHA != e.HeadSHA {
		rel := "repos/" + idx + ".bundle"
		abs, err := filepath.Abs(filepath.Join(o.OutDir, filepath.FromSlash(rel)))
		if err != nil {
			return e, nil, fmt.Errorf("snapshot: resolve bundle path: %w", err)
		}
		if _, err := git(ctx, o.RepoDir, "bundle", "create", abs, e.UpstreamSHA+"..HEAD"); err != nil {
			return e, nil, fmt.Errorf("snapshot: create bundle: %w", err)
		}
		e.Bundle = rel
	}

	patch, err := gitRaw(ctx, o.RepoDir, "diff", "--binary", "HEAD")
	if err != nil {
		return e, nil, fmt.Errorf("snapshot: create patch: %w", err)
	}
	if len(patch) > 0 {
		if !bytes.HasSuffix(patch, []byte("\n")) {
			patch = append(patch, '\n')
		}
		rel := "repos/" + idx + ".patch"
		if err := os.WriteFile(filepath.Join(o.OutDir, filepath.FromSlash(rel)), patch, 0o644); err != nil {
			return e, nil, fmt.Errorf("snapshot: write patch: %w", err)
		}
		e.Patch = rel
	}

	lsOut, err := gitRaw(ctx, o.RepoDir, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return e, nil, fmt.Errorf("snapshot: list untracked files: %w", err)
	}
	var untracked []string
	for _, p := range strings.Split(string(lsOut), "\x00") {
		if p != "" {
			untracked = append(untracked, p)
		}
	}
	if len(untracked) > 0 {
		rel := "repos/" + idx + ".untracked.tar.zst"
		if _, err := PackFiles(filepath.Join(o.OutDir, filepath.FromSlash(rel)), o.RepoDir, untracked); err != nil {
			return e, nil, fmt.Errorf("snapshot: pack untracked files: %w", err)
		}
		e.Untracked = rel
	}

	if sub, err := git(ctx, o.RepoDir, "submodule", "status"); err == nil && sub != "" {
		skipped = append(skipped, Skipped{Path: o.Path, Reason: "submodules"})
	}
	if data, err := os.ReadFile(filepath.Join(o.RepoDir, ".gitattributes")); err == nil &&
		bytes.Contains(data, []byte("filter=lfs")) {
		skipped = append(skipped, Skipped{Path: o.Path, Reason: "lfs"})
	}
	branches, err := git(ctx, o.RepoDir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return e, nil, fmt.Errorf("snapshot: list branches: %w", err)
	}
	for _, b := range strings.Split(branches, "\n") {
		if b == "" || b == e.Branch {
			continue
		}
		remotes, err := git(ctx, o.RepoDir, "branch", "-r", "--contains", b)
		if err != nil {
			return e, nil, fmt.Errorf("snapshot: check branch %s: %w", b, err)
		}
		if remotes == "" {
			skipped = append(skipped, Skipped{Path: o.Path + "@" + b, Reason: "other_branch"})
		}
	}
	return e, skipped, nil
}

// RestoreRepo restores the state in e into repoDir, which is a clone at
// e.UpstreamSHA that exists already. partDir is the directory that holds the
// files of the entry.
func RestoreRepo(ctx context.Context, repoDir string, e RepoEntry, partDir string) error {
	commit := e.HeadSHA
	if e.Bundle != "" {
		bundle, err := filepath.Abs(filepath.Join(partDir, filepath.FromSlash(e.Bundle)))
		if err != nil {
			return fmt.Errorf("snapshot: resolve bundle path: %w", err)
		}
		if _, err := git(ctx, repoDir, "fetch", bundle, "HEAD"); err != nil {
			return fmt.Errorf("snapshot: fetch bundle: %w", err)
		}
		commit = "FETCH_HEAD"
	}
	var err error
	if e.Branch != "" {
		_, err = git(ctx, repoDir, "checkout", "-q", "-B", e.Branch, commit)
	} else {
		_, err = git(ctx, repoDir, "checkout", "-q", "--detach", commit)
	}
	if err != nil {
		return fmt.Errorf("snapshot: check out: %w", err)
	}
	got, err := git(ctx, repoDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("snapshot: read HEAD: %w", err)
	}
	if got != e.HeadSHA {
		return fmt.Errorf("snapshot: HEAD is %s after restore, expected %s", got, e.HeadSHA)
	}

	if e.Patch != "" {
		patch, err := filepath.Abs(filepath.Join(partDir, filepath.FromSlash(e.Patch)))
		if err != nil {
			return fmt.Errorf("snapshot: resolve patch path: %w", err)
		}
		if _, err := git(ctx, repoDir, "apply", "--3way", "--whitespace=nowarn", patch); err != nil {
			if _, err2 := git(ctx, repoDir, "apply", "--whitespace=nowarn", patch); err2 != nil {
				return fmt.Errorf("snapshot: apply patch: %w", err)
			}
		}
		if _, err := git(ctx, repoDir, "reset", "-q"); err != nil {
			return fmt.Errorf("snapshot: unstage changes: %w", err)
		}
	}

	if e.Untracked != "" {
		if err := Unpack(filepath.Join(partDir, filepath.FromSlash(e.Untracked)), repoDir); err != nil {
			return fmt.Errorf("snapshot: unpack untracked files: %w", err)
		}
	}
	return nil
}
