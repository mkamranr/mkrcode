package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"mkrcode/internal/skills"
)

// skillTool loads the full instructions of a named skill.
//
// Skills are advertised to the model by name and description only; this is
// how the body is fetched when one actually applies. Keeping bodies out of
// the system prompt is what allows a workspace to carry many skills without
// consuming the context window.
type skillTool struct {
	opt Options
}

func (t *skillTool) Name() string { return "skill" }

func (t *skillTool) Description() string {
	return "Load the full instructions for one of the available skills. " +
		"Skills are listed in your system prompt with a short description each. " +
		"Call this when a skill's description matches the task, then follow the " +
		"instructions it returns."
}

// Mutating reports false: loading instructions changes nothing. Whatever
// the instructions then lead the model to do is gated normally.
func (t *skillTool) Mutating() bool { return false }

func (t *skillTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "name": {"type": "string", "description": "The skill to load, as listed in the system prompt."}
  },
  "required": ["name"]
}`)
}

type skillArgs struct {
	Name string `json:"name"`
}

func (t *skillTool) Describe(args json.RawMessage) string {
	var a skillArgs
	if err := decode(args, &a); err != nil {
		return "skill " + string(args)
	}
	return "load skill " + a.Name
}

func (t *skillTool) Run(_ context.Context, args json.RawMessage) (Result, error) {
	var a skillArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if t.opt.Skills == nil || t.opt.Skills.Len() == 0 {
		return Errorf("no skills are installed in this workspace"), nil
	}

	sk, ok := t.opt.Skills.Get(a.Name)
	if !ok {
		return Errorf("no skill named %q; available skills are: %s",
			a.Name, strings.Join(t.opt.Skills.Names(), ", ")), nil
	}

	body, err := sk.Body()
	if err != nil {
		return Errorf("%v", err), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Skill: %s\n\n%s\n", sk.Name, body)

	if files := sk.SupportingFiles(); len(files) > 0 {
		fmt.Fprintf(&b, "\nThis skill's directory also contains: %s\n"+
			"Read them with read_file using paths under %s if the instructions refer to them.\n",
			strings.Join(files, ", "), sk.Dir)
	}
	if note := translationNote(body); note != "" {
		b.WriteString("\n" + note + "\n")
	}

	out, _ := truncate(b.String(), t.opt.MaxOutputBytes)
	return Result{
		Content: out,
		Display: fmt.Sprintf("loaded skill %s (%s)", sk.Name, sk.Source),
	}, nil
}

// foreignToolNames maps tool names used by other agents to this program's
// equivalents. Skills written elsewhere refer to tools by these names.
var foreignToolNames = map[string]string{
	"Read":      "read_file",
	"Write":     "write_file",
	"Edit":      "edit_file",
	"MultiEdit": "edit_file",
	"Bash":      "exec",
	"Shell":     "exec",
	"Grep":      "grep",
	"Glob":      "glob",
	"LS":        "list_dir",
	"Task":      "task",
	"Agent":     "task",
}

// translationNote returns guidance when a skill refers to tools by names
// this program does not use.
//
// The note is appended rather than the body being rewritten. Substituting
// names inside the markdown would corrupt code samples that legitimately
// contain words like "Read" or "Task", and a skill that silently had its
// examples mangled is worse than one that needs a footnote. Appending is
// non-destructive, visible in the audit log, and models follow it reliably.
func translationNote(body string) string {
	seen := map[string]string{}
	for foreign, local := range foreignToolNames {
		if mentionsTool(body, foreign) {
			seen[foreign] = local
		}
	}
	if len(seen) == 0 {
		return ""
	}

	pairs := make([]string, 0, len(seen))
	for foreign, local := range seen {
		pairs = append(pairs, foreign+" = "+local)
	}
	sort.Strings(pairs)

	return "[Note: this skill refers to tools by names used elsewhere. In this " +
		"environment: " + strings.Join(pairs, ", ") + ". " +
		"Use the names on the right.]"
}

// mentionsTool reports whether body refers to a tool by name in a way that
// looks like a tool reference rather than ordinary prose.
//
// Requiring an adjacent cue keeps the common English words in this list
// ("Read", "Write", "Task", "Edit") from triggering on sentences like
// "Read the specification first".
func mentionsTool(body, name string) bool {
	cues := []string{
		name + " tool",
		"tool " + name,
		"`" + name + "`",
		name + "(",
		"use " + name,
		"Use " + name,
		"the " + name + " ",
	}
	for _, c := range cues {
		if strings.Contains(body, c) {
			return true
		}
	}
	return false
}

// SkillPrompt renders the available skills for the system prompt.
//
// Only names and descriptions appear here. This function is the reason the
// feature scales: its output grows by one short line per skill, not by the
// size of the skills themselves.
func SkillPrompt(set *skills.Set) string {
	if set == nil || set.Len() == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Skills\n\n")
	b.WriteString("These are instruction packs for particular kinds of work. " +
		"When one matches the task, call the skill tool with its name to load " +
		"the full instructions, then follow them.\n\n")
	for _, sk := range set.All() {
		fmt.Fprintf(&b, "- **%s** — %s\n", sk.Name, sk.Description)
	}
	return b.String()
}
