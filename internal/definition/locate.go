package definition

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// KeyLine returns the 1-based line of the key path in a definition file, or
// 0 when the file does not parse or the path is not in it. file decides the
// format: a ".md" file is read as Markdown with YAML frontmatter, any other
// file as YAML. The path uses the form of ValidationError.Field, for example
// "environmentId", "mcpServers.docs.managedId" or
// "triggers[0].filter.webhook_ids[0]". For a mapping key the line is the line
// of the key; for a list index it is the line of the item.
//
// The preflight uses it to give a line to the errors it finds after the
// compile, when the YAML nodes are no longer available.
func KeyLine(file, content, path string) int {
	src, offset := content, 0
	if strings.HasSuffix(file, ".md") {
		fm, _, has, off, err := splitFrontmatter(content)
		if err != nil || !has {
			return 0
		}
		src, offset = fm, off
	}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil || len(root.Content) == 0 {
		return 0
	}
	n := root.Content[0]
	line := 0
	for _, seg := range splitKeyPath(path) {
		n = resolve(n)
		if n == nil {
			return 0
		}
		if seg.index >= 0 {
			if n.Kind != yaml.SequenceNode || seg.index >= len(n.Content) {
				return 0
			}
			n = n.Content[seg.index]
			line = n.Line
			continue
		}
		if n.Kind != yaml.MappingNode {
			return 0
		}
		found := false
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == seg.key {
				line, n, found = n.Content[i].Line, n.Content[i+1], true
				break
			}
		}
		if !found {
			return 0
		}
	}
	if line == 0 {
		return 0
	}
	return line + offset
}

type pathSeg struct {
	key   string
	index int // -1 for a mapping key
}

// splitKeyPath splits "a.b[0][1].c" into a, b, [0], [1], c.
func splitKeyPath(path string) []pathSeg {
	var out []pathSeg
	for _, part := range strings.Split(path, ".") {
		key, rest, _ := strings.Cut(part, "[")
		if key != "" {
			out = append(out, pathSeg{key: key, index: -1})
		}
		for rest != "" {
			num, tail, ok := strings.Cut(rest, "]")
			i, err := strconv.Atoi(num)
			if !ok || err != nil || i < 0 {
				return nil
			}
			out = append(out, pathSeg{index: i})
			rest = strings.TrimPrefix(tail, "[")
		}
	}
	return out
}
