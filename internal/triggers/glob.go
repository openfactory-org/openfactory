package triggers

import (
	"errors"
	"path"
	"strings"
)

// matchGlob matches a repository-relative path against a glob pattern.
//
// Patterns are split on "/". A "**" segment matches zero or more whole path
// segments; other segments use path.Match syntax ("*", "?", "[...]"), where
// "*" never crosses a "/". To match everything below a directory write
// "dir/**"; to match a file name at any depth write "**/name". Matching is
// case-sensitive.
func matchGlob(pattern, name string) bool {
	pattern = strings.TrimPrefix(pattern, "/")
	name = strings.TrimPrefix(name, "/")
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		p := pat[0]
		if p == "**" {
			// Collapse consecutive ** segments.
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(p, segs[0])
		if err != nil || !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

func validateGlob(pattern string) error {
	if pattern == "" {
		return errors.New("empty glob")
	}
	for _, seg := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return errors.New("\"**\" must be a whole path segment in " + pattern)
		}
		if _, err := path.Match(seg, ""); err != nil {
			return errors.New("malformed glob " + pattern)
		}
	}
	return nil
}
