package azuredevops

import (
	"fmt"
	"strings"
)

// Azure DevOps has no call for the unified diff of a file. The plugin reads
// the file at the merge base and at the head of the pull request and makes the
// diff itself.

// diffContext is the number of unchanged lines around a change.
const diffContext = 3

// maxDiffCells limits the table of the line comparison. Two longer middles
// are shown as one replaced block.
const maxDiffCells = 4_000_000

type diffOp struct {
	kind byte // ' ', '-' or '+'
	text string
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// diffOps returns the edit script of two line lists.
func diffOps(a, b []string) []diffOp {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	var ops []diffOp
	for _, l := range a[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	ma, mb := a[pre:len(a)-suf], b[pre:len(b)-suf]
	switch {
	case len(ma) == 0 || len(mb) == 0 || len(ma)*len(mb) > maxDiffCells:
		for _, l := range ma {
			ops = append(ops, diffOp{'-', l})
		}
		for _, l := range mb {
			ops = append(ops, diffOp{'+', l})
		}
	default:
		// Longest common subsequence of the middles.
		n, m := len(ma), len(mb)
		t := make([][]int32, n+1)
		for i := range t {
			t[i] = make([]int32, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if ma[i] == mb[j] {
					t[i][j] = t[i+1][j+1] + 1
				} else {
					t[i][j] = max(t[i+1][j], t[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && ma[i] == mb[j]:
				ops = append(ops, diffOp{' ', ma[i]})
				i++
				j++
			case i < n && (j == m || t[i+1][j] >= t[i][j+1]):
				// A removed line comes before an added one.
				ops = append(ops, diffOp{'-', ma[i]})
				i++
			default:
				ops = append(ops, diffOp{'+', mb[j]})
				j++
			}
		}
	}
	for _, l := range a[len(a)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// unifiedDiff returns the hunks of the diff of oldText and newText, without
// the file header. It is empty when the texts are the same.
func unifiedDiff(oldText, newText string) string {
	ops := diffOps(splitLines(oldText), splitLines(newText))
	// Mark the lines that a hunk shows: each change and its context.
	show := make([]bool, len(ops))
	changed := false
	for i, op := range ops {
		if op.kind == ' ' {
			continue
		}
		changed = true
		for k := max(0, i-diffContext); k <= min(len(ops)-1, i+diffContext); k++ {
			show[k] = true
		}
	}
	if !changed {
		return ""
	}
	var out strings.Builder
	oldLine, newLine := 1, 1
	for i := 0; i < len(ops); {
		if !show[i] {
			if ops[i].kind != '+' {
				oldLine++
			}
			if ops[i].kind != '-' {
				newLine++
			}
			i++
			continue
		}
		j := i
		oldN, newN := 0, 0
		for j < len(ops) && show[j] {
			if ops[j].kind != '+' {
				oldN++
			}
			if ops[j].kind != '-' {
				newN++
			}
			j++
		}
		oldStart, newStart := oldLine, newLine
		if oldN == 0 {
			oldStart--
		}
		if newN == 0 {
			newStart--
		}
		fmt.Fprintf(&out, "@@ -%s +%s @@\n", hunkRange(oldStart, oldN), hunkRange(newStart, newN))
		for _, op := range ops[i:j] {
			out.WriteByte(op.kind)
			out.WriteString(op.text)
			out.WriteByte('\n')
		}
		oldLine += oldN
		newLine += newN
		i = j
	}
	return out.String()
}

func hunkRange(start, n int) string {
	if n == 1 {
		return fmt.Sprint(start)
	}
	return fmt.Sprintf("%d,%d", start, n)
}
