package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSkill(t *testing.T, dir, name, content string) {
	t.Helper()
	d := filepath.Join(dir, name)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, FileName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const validSkill = `---
name: security-review
description: Use when reviewing a diff for security defects before merging.
---

# Security review

Work through the diff looking for injection, missing authorisation, and
unvalidated input.
`

func TestDiscoverFindsSkills(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".mkr", DirName), "security-review", validSkill)

	set, problems := Discover(ws, "")
	if len(problems) != 0 {
		t.Fatalf("unexpected problems: %v", problems)
	}
	if set.Len() != 1 {
		t.Fatalf("found %d skills, want 1", set.Len())
	}
	sk, ok := set.Get("security-review")
	if !ok {
		t.Fatal("skill not found by name")
	}
	if sk.Description == "" {
		t.Error("description was not parsed")
	}
	if sk.Source != SourceProject {
		t.Errorf("source = %q, want project", sk.Source)
	}

	body, err := sk.Body()
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	if !strings.Contains(body, "Work through the diff") {
		t.Errorf("body wrong: %q", body)
	}
	if strings.Contains(body, "---") || strings.Contains(body, "description:") {
		t.Errorf("frontmatter leaked into the body: %q", body)
	}
}

// Project skills shadow user skills of the same name, matching how
// configuration layers.
func TestProjectSkillShadowsUserSkill(t *testing.T) {
	ws := t.TempDir()
	userCfg := t.TempDir()

	writeSkill(t, filepath.Join(userCfg, DirName), "review", `---
name: review
description: The personal version.
---
personal body`)
	writeSkill(t, filepath.Join(ws, ".mkr", DirName), "review", `---
name: review
description: The project version.
---
project body`)

	set, problems := Discover(ws, userCfg)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if set.Len() != 1 {
		t.Fatalf("got %d skills, want 1 after shadowing", set.Len())
	}
	sk, _ := set.Get("review")
	if sk.Source != SourceProject {
		t.Errorf("source = %q, want the project version to win", sk.Source)
	}
	if !strings.Contains(sk.Description, "project") {
		t.Errorf("description = %q, want the project version", sk.Description)
	}
}

func TestUserSkillsAreFoundWhenNotShadowed(t *testing.T) {
	userCfg := t.TempDir()
	writeSkill(t, filepath.Join(userCfg, DirName), "personal", `---
name: personal
description: Only in the user directory.
---
body`)

	set, _ := Discover(t.TempDir(), userCfg)
	sk, ok := set.Get("personal")
	if !ok {
		t.Fatal("user skill not found")
	}
	if sk.Source != SourceUser {
		t.Errorf("source = %q, want user", sk.Source)
	}
}

// A malformed skill must be reported but must not prevent the other skills,
// or the session, from working.
func TestMalformedSkillIsReportedNotFatal(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".mkr", DirName)
	writeSkill(t, dir, "security-review", validSkill)

	bad := map[string]string{
		"no-frontmatter": "# Just markdown\n",
		"unclosed":       "---\nname: x\ndescription: y\n",
		"no-name":        "---\ndescription: missing a name\n---\nbody",
		"no-description": "---\nname: x\n---\nbody",
		"nested-yaml":    "---\nname: x\ndescription: y\nmeta:\n  key: value\n---\nbody",
		"list-yaml":      "---\nname: x\ndescription: y\ntags:\n- one\n---\nbody",
		"bad-name":       "---\nname: Has Spaces\ndescription: y\n---\nbody",
	}
	for n, c := range bad {
		writeSkill(t, dir, n, c)
	}

	set, problems := Discover(ws, "")
	if len(problems) != len(bad) {
		t.Errorf("got %d problems, want %d: %v", len(problems), len(bad), problems)
	}
	// Every problem must name the offending file, or the operator cannot
	// find it.
	for _, p := range problems {
		if !strings.Contains(p.Error(), FileName) {
			t.Errorf("problem does not name the file: %v", p)
		}
	}
	if set.Len() != 1 {
		t.Errorf("got %d valid skills, want the 1 good one to survive", set.Len())
	}
	if _, ok := set.Get("security-review"); !ok {
		t.Error("the valid skill was lost because others were malformed")
	}
}

func TestFrontmatterVariants(t *testing.T) {
	cases := []struct{ name, content, wantDesc string }{
		{"quoted", "---\nname: a\ndescription: \"quoted value\"\n---\nbody", "quoted value"},
		{"single quoted", "---\nname: a\ndescription: 'single'\n---\nbody", "single"},
		{"crlf", "---\r\nname: a\r\ndescription: windows line endings\r\n---\r\nbody", "windows line endings"},
		{"comment", "---\n# a comment\nname: a\ndescription: with a comment\n---\nbody", "with a comment"},
		{"blank lines", "---\n\nname: a\n\ndescription: with blanks\n\n---\nbody", "with blanks"},
		{"extra keys", "---\nname: a\ndescription: d\nversion: 2\nauthor: someone\n---\nbody", "d"},
		{"colon in value", "---\nname: a\ndescription: Use when X: do Y\n---\nbody", "Use when X: do Y"},
		{"bom", "\ufeff---\nname: a\ndescription: byte order mark\n---\nbody", "byte order mark"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ws := t.TempDir()
			writeSkill(t, filepath.Join(ws, ".mkr", DirName), "a", c.content)
			set, problems := Discover(ws, "")
			if len(problems) != 0 {
				t.Fatalf("problems: %v", problems)
			}
			sk, ok := set.Get("a")
			if !ok {
				t.Fatal("skill not found")
			}
			if sk.Description != c.wantDesc {
				t.Errorf("description = %q, want %q", sk.Description, c.wantDesc)
			}
		})
	}
}

// A description spanning lines would break the system prompt's list
// formatting, so whitespace is collapsed.
func TestDescriptionIsSingleLine(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".mkr", DirName), "a",
		"---\nname: a\ndescription: \"one   two\tthree\"\n---\nbody")
	set, _ := Discover(ws, "")
	sk, _ := set.Get("a")
	if strings.ContainsAny(sk.Description, "\n\t") || strings.Contains(sk.Description, "  ") {
		t.Errorf("description = %q, want whitespace collapsed", sk.Description)
	}
}

func TestLongDescriptionIsTruncated(t *testing.T) {
	ws := t.TempDir()
	long := strings.Repeat("x", 1000)
	writeSkill(t, filepath.Join(ws, ".mkr", DirName), "a",
		fmt.Sprintf("---\nname: a\ndescription: %s\n---\nbody", long))

	set, _ := Discover(ws, "")
	sk, _ := set.Get("a")
	if len([]rune(sk.Description)) > maxDescriptionRunes+3 {
		t.Errorf("description is %d runes; it is rendered for every session and must be capped", len([]rune(sk.Description)))
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	ws := t.TempDir()
	huge := "---\nname: a\ndescription: d\n---\n" + strings.Repeat("x", MaxBodyBytes+1)
	writeSkill(t, filepath.Join(ws, ".mkr", DirName), "a", huge)

	set, _ := Discover(ws, "")
	sk, ok := set.Get("a")
	if !ok {
		t.Fatal("skill should still be discovered; the size limit applies on load")
	}
	if _, err := sk.Body(); err == nil {
		t.Error("an oversized body should be refused with an explanation")
	}
}

func TestNamesAreSorted(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".mkr", DirName)
	for _, n := range []string{"zebra", "alpha", "middle"} {
		writeSkill(t, dir, n, fmt.Sprintf("---\nname: %s\ndescription: d\n---\nbody", n))
	}
	set, _ := Discover(ws, "")
	got := strings.Join(set.Names(), ",")
	if got != "alpha,middle,zebra" {
		t.Errorf("names = %q, want them sorted", got)
	}
}

func TestDiscoverMissingDirectoriesIsClean(t *testing.T) {
	set, problems := Discover(t.TempDir(), t.TempDir())
	if len(problems) != 0 {
		t.Errorf("missing skills directories should not be a problem: %v", problems)
	}
	if set.Len() != 0 {
		t.Errorf("got %d skills, want 0", set.Len())
	}
}

func TestNilSetIsSafe(t *testing.T) {
	var s *Set
	if s.Len() != 0 || s.All() != nil || s.Names() != nil {
		t.Error("a nil Set must behave as empty")
	}
	if _, ok := s.Get("x"); ok {
		t.Error("a nil Set must find nothing")
	}
}

func TestSupportingFilesAreListed(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".mkr", DirName)
	writeSkill(t, dir, "security-review", validSkill)
	os.WriteFile(filepath.Join(dir, "security-review", "checklist.md"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "security-review", "template.txt"), []byte("x"), 0o644)

	set, _ := Discover(ws, "")
	sk, _ := set.Get("security-review")
	files := sk.SupportingFiles()
	if len(files) != 2 {
		t.Fatalf("got %v, want the two supporting files", files)
	}
	for _, f := range files {
		if f == FileName {
			t.Error("SKILL.md must not be listed as a supporting file")
		}
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "code-review", "test_gen", "x9"} {
		if err := validateName(ok); err != nil {
			t.Errorf("validateName(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"Has Space", "UPPER", "path/traversal", "dot.name", "emoji😀", strings.Repeat("x", 65)} {
		if err := validateName(bad); err == nil {
			t.Errorf("validateName(%q) = nil, want an error", bad)
		}
	}
}

// A directory whose name disagrees with the declared name is refused, so a
// directory listing is an accurate inventory of what is installed.
func TestDirectoryNameMustMatchDeclaredName(t *testing.T) {
	ws := t.TempDir()
	writeSkill(t, filepath.Join(ws, ".mkr", DirName), "innocuous-name", `---
name: something-else
description: The directory and the declared name disagree.
---
body`)

	set, problems := Discover(ws, "")
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0].Error(), "must match") {
		t.Errorf("problem = %v, want it to explain the mismatch", problems[0])
	}
	if set.Len() != 0 {
		t.Error("the mismatched skill should not be registered")
	}
}
