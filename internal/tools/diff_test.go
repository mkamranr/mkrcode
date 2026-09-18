package tools

import (
	"strings"
	"testing"
)

func TestUnifiedDiffIdenticalIsEmpty(t *testing.T) {
	if got := UnifiedDiff("a.txt", "same\n", "same\n"); got != "" {
		t.Errorf("identical input should produce no diff, got %q", got)
	}
}

func TestUnifiedDiffRecordsChange(t *testing.T) {
	got := UnifiedDiff("a.txt", "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	if !strings.Contains(got, "--- a/a.txt") || !strings.Contains(got, "+++ b/a.txt") {
		t.Errorf("missing file headers:\n%s", got)
	}
	if !strings.Contains(got, "-two") {
		t.Errorf("missing the removed line:\n%s", got)
	}
	if !strings.Contains(got, "+TWO") {
		t.Errorf("missing the added line:\n%s", got)
	}
	if !strings.Contains(got, " one") {
		t.Errorf("missing surrounding context:\n%s", got)
	}
}

func TestUnifiedDiffCreationAndDeletion(t *testing.T) {
	created := UnifiedDiff("n.txt", "", "hello\nworld\n")
	if !strings.Contains(created, "+hello") || !strings.Contains(created, "+world") {
		t.Errorf("creation diff wrong:\n%s", created)
	}
	if strings.Contains(created, "-") && strings.Contains(created, "-hello") {
		t.Errorf("creation diff should have no deletions:\n%s", created)
	}

	deleted := UnifiedDiff("n.txt", "hello\n", "")
	if !strings.Contains(deleted, "-hello") {
		t.Errorf("deletion diff wrong:\n%s", deleted)
	}
}

// Unchanged regions far from any edit must not appear, or the audit log
// fills with noise.
func TestUnifiedDiffOmitsDistantContext(t *testing.T) {
	var before strings.Builder
	for i := 0; i < 50; i++ {
		before.WriteString("unchanged\n")
	}
	after := before.String() + "added\n"

	got := UnifiedDiff("a.txt", before.String(), after)
	if n := strings.Count(got, "unchanged"); n > 4 {
		t.Errorf("diff carried %d context lines, want at most 4:\n%s", n, got)
	}
	if !strings.Contains(got, "+added") {
		t.Errorf("diff lost the change:\n%s", got)
	}
}

// A very large file must degrade to a summary rather than allocating an
// enormous LCS table.
func TestUnifiedDiffLargeFileDegrades(t *testing.T) {
	var big strings.Builder
	for i := 0; i < 5000; i++ {
		big.WriteString("line\n")
	}
	got := UnifiedDiff("big.txt", big.String(), big.String()+"one more\n")
	if !strings.Contains(got, "too large") {
		t.Errorf("expected a summary for a large file, got:\n%s", got[:min(len(got), 200)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
