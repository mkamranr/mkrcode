package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkamranr/mkrcode/internal/fsjail"
	"github.com/mkamranr/mkrcode/internal/skills"
)

func newSkillTools(t *testing.T, n int) (*Registry, *skills.Set, string) {
	t.Helper()
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	dir := filepath.Join(ws, ".mkr", skills.DirName)

	for i := 0; i < n; i++ {
		name := fmt.Sprintf("skill-%02d", i)
		d := filepath.Join(dir, name)
		os.MkdirAll(d, 0o755)
		body := strings.Repeat("Detailed instructions that must not reach the system prompt. ", 200)
		os.WriteFile(filepath.Join(d, skills.FileName), []byte(fmt.Sprintf(
			"---\nname: %s\ndescription: Use when doing thing number %d.\n---\n\n%s", name, i, body)), 0o644)
	}

	set, problems := skills.Discover(ws, "")
	if len(problems) != 0 {
		t.Fatalf("discovery problems: %v", problems)
	}
	j, err := fsjail.New(ws)
	if err != nil {
		t.Fatal(err)
	}
	return Standard(Options{Jail: j, Shell: DetectShell(), MaxOutputBytes: 256 << 10, Skills: set}), set, ws
}

// The property the design rests on: many skills must cost only a few hundred
// tokens of system prompt, not the size of their bodies. If this regresses,
// installing skills silently consumes the context window.
func TestSkillPromptIsProgressiveDisclosure(t *testing.T) {
	_, set, _ := newSkillTools(t, 20)

	prompt := SkillPrompt(set)
	if prompt == "" {
		t.Fatal("no skill prompt was produced")
	}

	// Every skill must be advertised by name.
	for _, sk := range set.All() {
		if !strings.Contains(prompt, sk.Name) {
			t.Errorf("skill %q is not listed in the prompt", sk.Name)
		}
	}
	// But no body may appear.
	if strings.Contains(prompt, "Detailed instructions") {
		t.Error("a skill body leaked into the system prompt; progressive disclosure is broken")
	}
	// Twenty skills with ~12KB bodies each would be ~240KB if inlined.
	if len(prompt) > 4000 {
		t.Errorf("skill prompt is %d bytes for 20 skills; it must stay proportional to names and descriptions, not bodies", len(prompt))
	}
}

func TestSkillPromptEmptyWhenNoSkills(t *testing.T) {
	if got := SkillPrompt(nil); got != "" {
		t.Errorf("SkillPrompt(nil) = %q, want empty", got)
	}
	empty, _ := skills.Discover(t.TempDir(), "")
	if got := SkillPrompt(empty); got != "" {
		t.Errorf("SkillPrompt(empty) = %q, want empty", got)
	}
}

// The skill tool must only be registered when there is something to load;
// advertising a tool that can only fail wastes a turn.
func TestSkillToolRegisteredOnlyWhenSkillsExist(t *testing.T) {
	reg, _, _ := newSkillTools(t, 2)
	if _, ok := reg.Get("skill"); !ok {
		t.Error("the skill tool should be registered when skills exist")
	}

	ws := t.TempDir()
	j, _ := fsjail.New(ws)
	empty, _ := skills.Discover(ws, "")
	bare := Standard(Options{Jail: j, Skills: empty})
	if _, ok := bare.Get("skill"); ok {
		t.Error("the skill tool must not be registered when no skills exist")
	}
}

func TestSkillToolLoadsBody(t *testing.T) {
	reg, _, _ := newSkillTools(t, 3)
	res := run(t, reg, "skill", map[string]any{"name": "skill-01"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "Detailed instructions") {
		t.Errorf("the body was not returned: %q", res.Content[:min(200, len(res.Content))])
	}
	if !strings.Contains(res.Content, "skill-01") {
		t.Error("the returned content should name the skill")
	}
}

func TestSkillToolIsReadOnly(t *testing.T) {
	reg, _, _ := newSkillTools(t, 1)
	tool, _ := reg.Get("skill")
	if tool.Mutating() {
		t.Error("loading instructions changes nothing and must not be gated as mutating")
	}
}

func TestSkillToolUnknownNameListsAvailable(t *testing.T) {
	reg, _, _ := newSkillTools(t, 3)
	res := run(t, reg, "skill", map[string]any{"name": "nonexistent"})
	if !res.IsError {
		t.Fatal("an unknown skill must be an error")
	}
	if !strings.Contains(res.Content, "skill-00") {
		t.Errorf("the error should list what is available: %q", res.Content)
	}
}

// Third-party skills refer to tools by other names. The note must be
// appended and the original markdown left byte-for-byte intact, because
// rewriting it would corrupt code samples.
func TestTranslationNoteIsAppendedNotSubstituted(t *testing.T) {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	d := filepath.Join(ws, ".mkr", skills.DirName, "ported")
	os.MkdirAll(d, 0o755)

	const body = `Use the Read tool to open the file, then the Bash tool to run tests.

` + "```python\n" + `def Read(path):        # a legitimate code sample
    return open(path).read()
` + "```\n"

	os.WriteFile(filepath.Join(d, skills.FileName),
		[]byte("---\nname: ported\ndescription: A skill written for another agent.\n---\n\n"+body), 0o644)

	set, problems := skills.Discover(ws, "")
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	j, _ := fsjail.New(ws)
	reg := Standard(Options{Jail: j, MaxOutputBytes: 256 << 10, Skills: set})

	res := run(t, reg, "skill", map[string]any{"name": "ported"})
	if res.IsError {
		t.Fatalf("error: %s", res.Content)
	}

	// The note is present and maps the names it found.
	if !strings.Contains(res.Content, "Read = read_file") {
		t.Errorf("translation note missing the Read mapping:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "Bash = exec") {
		t.Errorf("translation note missing the Bash mapping:\n%s", res.Content)
	}

	// Critically, the original text is unaltered — the code sample still
	// defines Read, not read_file.
	if !strings.Contains(res.Content, "def Read(path):") {
		t.Error("the code sample was rewritten; skill text must never be substituted")
	}
	if !strings.Contains(res.Content, "Use the Read tool") {
		t.Error("the prose was rewritten; skill text must never be substituted")
	}
}

// Ordinary English must not trigger the note, or every skill gets a
// confusing footnote.
func TestTranslationNoteNotTriggeredByProse(t *testing.T) {
	for _, body := range []string{
		"Read the specification before starting.",
		"Write clear commit messages.",
		"Edit carefully and review your work.",
		"This task requires care.",
		"Agent behaviour should be predictable.",
	} {
		if note := translationNote(body); note != "" {
			t.Errorf("prose %q wrongly produced a note: %s", body, note)
		}
	}
}

func TestTranslationNoteTriggersOnRealReferences(t *testing.T) {
	for _, body := range []string{
		"Use the Read tool to open it.",
		"Call `Bash` to run the suite.",
		"Invoke Grep( pattern ) across the tree.",
		"use Glob to find the files",
	} {
		if note := translationNote(body); note == "" {
			t.Errorf("tool reference %q produced no note", body)
		}
	}
}

func TestSkillToolListsSupportingFiles(t *testing.T) {
	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	d := filepath.Join(ws, ".mkr", skills.DirName, "withfiles")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, skills.FileName),
		[]byte("---\nname: withfiles\ndescription: d\n---\nSee checklist.md."), 0o644)
	os.WriteFile(filepath.Join(d, "checklist.md"), []byte("items"), 0o644)

	set, _ := skills.Discover(ws, "")
	j, _ := fsjail.New(ws)
	reg := Standard(Options{Jail: j, MaxOutputBytes: 256 << 10, Skills: set})

	res := run(t, reg, "skill", map[string]any{"name": "withfiles"})
	if !strings.Contains(res.Content, "checklist.md") {
		t.Errorf("supporting files were not mentioned:\n%s", res.Content)
	}
}

func TestSkillSchemaIsValid(t *testing.T) {
	reg, _, _ := newSkillTools(t, 1)
	tool, _ := reg.Get("skill")
	if !json.Valid(tool.Schema()) {
		t.Error("the skill tool schema is not valid JSON")
	}
	if tool.Describe(json.RawMessage(`{"name":"x"}`)) == "" {
		t.Error("Describe must render something for the approval prompt and audit log")
	}
}
