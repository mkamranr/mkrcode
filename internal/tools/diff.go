package tools

import (
	"fmt"
	"strings"
)

// UnifiedDiff renders a unified diff between before and after.
//
// The audit log records what each approved edit actually changed, so the
// diff must be produced by this program rather than by shelling out to an
// external tool that may not exist on a locked-down workstation.
//
// The algorithm is a standard longest-common-subsequence walk. Inputs are
// bounded by the tools' own size caps, so its quadratic memory use is
// acceptable; beyond the bound it degrades to a summary rather than
// allocating without limit.
func UnifiedDiff(path, before, after string) string {
	if before == after {
		return ""
	}
	a := splitLines(before)
	b := splitLines(after)

	// Guard against pathological inputs: an LCS table over two very large
	// files would allocate more memory than the diff is worth.
	const maxLines = 4000
	if len(a) > maxLines || len(b) > maxLines {
		return fmt.Sprintf("--- a/%s\n+++ b/%s\n@@ summary @@\n-%d lines\n+%d lines\n(file too large to diff in full)\n",
			path, path, len(a), len(b))
	}

	ops := diffOps(a, b)
	hunks := groupHunks(ops, 3)
	if len(hunks) == 0 {
		return ""
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "--- a/%s\n+++ b/%s\n", path, path)
	for _, h := range hunks {
		fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", h.aStart, h.aCount, h.bStart, h.bCount)
		for _, op := range h.ops {
			switch op.kind {
			case opEqual:
				sb.WriteString(" " + op.text + "\n")
			case opDelete:
				sb.WriteString("-" + op.text + "\n")
			case opInsert:
				sb.WriteString("+" + op.text + "\n")
			}
		}
	}
	return sb.String()
}

// splitLines splits s into lines, dropping the empty element a trailing
// newline produces.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

type opKind int

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

type diffOp struct {
	kind opKind
	text string
	// aLine and bLine are 1-based line numbers, zero when not present on
	// that side.
	aLine, bLine int
}

// diffOps computes the edit script between a and b.
func diffOps(a, b []string) []diffOp {
	// lcs[i][j] is the length of the longest common subsequence of a[i:]
	// and b[j:].
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var ops []diffOp
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{opEqual, a[i], i + 1, j + 1})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			ops = append(ops, diffOp{opDelete, a[i], i + 1, 0})
			i++
		default:
			ops = append(ops, diffOp{opInsert, b[j], 0, j + 1})
			j++
		}
	}
	for ; i < len(a); i++ {
		ops = append(ops, diffOp{opDelete, a[i], i + 1, 0})
	}
	for ; j < len(b); j++ {
		ops = append(ops, diffOp{opInsert, b[j], 0, j + 1})
	}
	return ops
}

type hunk struct {
	aStart, aCount int
	bStart, bCount int
	ops            []diffOp
}

// groupHunks collects changed regions with up to ctx lines of surrounding
// context, the way a unified diff presents them.
func groupHunks(ops []diffOp, ctx int) []hunk {
	changed := make([]bool, len(ops))
	any := false
	for i, op := range ops {
		if op.kind != opEqual {
			changed[i] = true
			any = true
		}
	}
	if !any {
		return nil
	}

	// Mark context lines around each change.
	keep := make([]bool, len(ops))
	for i, c := range changed {
		if !c {
			continue
		}
		lo, hi := i-ctx, i+ctx
		if lo < 0 {
			lo = 0
		}
		if hi >= len(ops) {
			hi = len(ops) - 1
		}
		for k := lo; k <= hi; k++ {
			keep[k] = true
		}
	}

	var hunks []hunk
	i := 0
	for i < len(ops) {
		if !keep[i] {
			i++
			continue
		}
		start := i
		for i < len(ops) && keep[i] {
			i++
		}
		hunks = append(hunks, buildHunk(ops[start:i]))
	}
	return hunks
}

// buildHunk computes the line ranges for one contiguous run of operations.
func buildHunk(ops []diffOp) hunk {
	h := hunk{ops: ops}
	for _, op := range ops {
		if op.aLine > 0 {
			if h.aStart == 0 {
				h.aStart = op.aLine
			}
			h.aCount++
		}
		if op.bLine > 0 {
			if h.bStart == 0 {
				h.bStart = op.bLine
			}
			h.bCount++
		}
	}
	// A pure insertion has no "a" lines; unified diff writes the position
	// it was inserted after, with a zero count.
	if h.aStart == 0 && len(ops) > 0 {
		h.aStart = 1
	}
	if h.bStart == 0 && len(ops) > 0 {
		h.bStart = 1
	}
	return h
}
