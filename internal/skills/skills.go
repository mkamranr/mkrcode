// Package skills discovers reusable instruction packs stored as markdown.
//
// A skill is a directory containing SKILL.md with frontmatter naming and
// describing it. The format matches what open-source skill collections
// already use, so third-party skills can be dropped in with little editing.
//
// Only a skill's name and description are put in front of the model at the
// start of a session; the body is fetched on demand. That progressive
// disclosure is what makes the feature affordable: twenty skills cost a few
// hundred tokens of system prompt rather than tens of thousands, so the
// context budgeting the agent relies on is not quietly undermined.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the file that defines a skill.
const FileName = "SKILL.md"

// DirName is the directory skills are discovered under, relative to the
// workspace and to the user configuration directory.
const DirName = "skills"

// MaxBodyBytes caps a loaded skill body. A skill is guidance, not a data
// file, and one large enough to crowd out the conversation is a mistake
// worth reporting rather than silently honouring.
const MaxBodyBytes = 64 << 10

// maxDescriptionRunes caps what goes into the system prompt. Descriptions
// are listed for every skill, so an essay in one skill's frontmatter would
// tax every session.
const maxDescriptionRunes = 400

// Source records where a skill was found.
type Source string

// The places a skill can come from.
const (
	// SourceProject is the workspace's own .mkr/skills directory.
	SourceProject Source = "project"
	// SourceUser is the per-user skills directory.
	SourceUser Source = "user"
)

// Skill is one discovered instruction pack. The body is not held in memory;
// it is read when the skill is actually used.
type Skill struct {
	// Name is the identifier the model asks for.
	Name string
	// Description tells the model when the skill applies.
	Description string
	// Path is the SKILL.md file.
	Path string
	// Dir is the skill's directory, which may hold supporting files.
	Dir string
	// Source is where it was found.
	Source Source
}

// Set is the skills available in a session.
type Set struct {
	byName map[string]Skill
	order  []string
}

// Len reports how many skills are available.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.byName)
}

// Get returns a skill by name.
func (s *Set) Get(name string) (Skill, bool) {
	if s == nil {
		return Skill{}, false
	}
	sk, ok := s.byName[strings.ToLower(strings.TrimSpace(name))]
	return sk, ok
}

// All returns every skill, ordered by name.
func (s *Set) All() []Skill {
	if s == nil {
		return nil
	}
	out := make([]Skill, 0, len(s.order))
	for _, n := range s.order {
		out = append(out, s.byName[n])
	}
	return out
}

// Names returns the available skill names, ordered.
func (s *Set) Names() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.order...)
}

// Discover finds skills in the workspace and the user configuration
// directory. Either may be empty to skip it.
//
// A malformed skill is reported but does not fail discovery: one bad file
// in a shared repository must not prevent a developer from starting a
// session. The returned problems are surfaced to the operator as warnings.
func Discover(workspace, userConfigDir string) (*Set, []error) {
	set := &Set{byName: map[string]Skill{}}
	var problems []error

	// User skills load first so a project skill of the same name replaces
	// them, matching how configuration layers.
	if userConfigDir != "" {
		problems = append(problems, set.scan(filepath.Join(userConfigDir, DirName), SourceUser)...)
	}
	if workspace != "" {
		problems = append(problems, set.scan(filepath.Join(workspace, ".mkr", DirName), SourceProject)...)
	}

	set.order = make([]string, 0, len(set.byName))
	for n := range set.byName {
		set.order = append(set.order, n)
	}
	sort.Strings(set.order)
	return set, problems
}

// scan reads one skills directory, replacing any earlier skill of the same
// name.
func (s *Set) scan(dir string, source Source) []error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// A missing skills directory is the normal case, not a problem.
		return nil
	}

	var problems []error
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(dir, e.Name(), FileName)
		sk, err := parseFile(path)
		if err == nil && sk.Name != strings.ToLower(e.Name()) {
			// The directory name and the declared name must agree, so a
			// skill is always addressable by where it lives. Allowing them
			// to differ makes a directory listing a misleading inventory of
			// what is actually installed.
			err = fmt.Errorf("%s: declares name %q but lives in directory %q; they must match",
				path, sk.Name, e.Name())
		}
		if err != nil {
			if os.IsNotExist(err) {
				continue // a directory that is not a skill
			}
			problems = append(problems, err)
			continue
		}
		sk.Source = source
		sk.Dir = filepath.Join(dir, e.Name())
		s.byName[sk.Name] = sk
	}
	return problems
}

// parseFile reads the frontmatter of a SKILL.md.
func parseFile(path string) (Skill, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	meta, _, err := splitFrontmatter(string(b))
	if err != nil {
		return Skill{}, fmt.Errorf("%s: %w", path, err)
	}

	name := strings.ToLower(strings.TrimSpace(meta["name"]))
	if name == "" {
		return Skill{}, fmt.Errorf("%s: frontmatter is missing a name", path)
	}
	if err := validateName(name); err != nil {
		return Skill{}, fmt.Errorf("%s: %w", path, err)
	}
	desc := strings.TrimSpace(meta["description"])
	if desc == "" {
		return Skill{}, fmt.Errorf("%s: frontmatter is missing a description; "+
			"the description is how the model decides when to use the skill", path)
	}
	if r := []rune(desc); len(r) > maxDescriptionRunes {
		desc = string(r[:maxDescriptionRunes]) + "..."
	}
	// Descriptions are rendered into the system prompt as list items, so a
	// newline would break the formatting.
	desc = strings.Join(strings.Fields(desc), " ")

	return Skill{Name: name, Description: desc, Path: path}, nil
}

// validateName rejects names that would be unsafe or confusing. Skill names
// are rendered into the system prompt and matched from model output, so
// they are restricted to an unambiguous character set.
func validateName(name string) error {
	if len(name) > 64 {
		return fmt.Errorf("skill name %q is too long (max 64 characters)", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
		default:
			return fmt.Errorf("skill name %q contains %q; use lowercase letters, digits, dashes and underscores", name, r)
		}
	}
	return nil
}

// splitFrontmatter separates the leading --- block from the body.
//
// Only simple "key: value" scalars are supported. A real YAML parser would
// mean a third-party dependency, and the empty module graph is what lets
// this binary ship as a single file. Anything more complex is rejected with
// a clear message rather than silently misread, which is the important
// property: a skill whose frontmatter was half-understood is worse than one
// that refuses to load.
func splitFrontmatter(content string) (map[string]string, string, error) {
	rest, ok := trimFrontmatterOpen(content)
	if !ok {
		return nil, "", fmt.Errorf("file does not begin with a --- frontmatter block")
	}

	meta := map[string]string{}
	lines := strings.Split(rest, "\n")
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)

		if trimmed == "---" {
			body := strings.Join(lines[i+1:], "\n")
			return meta, strings.TrimPrefix(body, "\n"), nil
		}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		// Line numbers count the opening --- as line 1.
		lineNo := i + 2
		if line != trimmed {
			return nil, "", fmt.Errorf("line %d: indented frontmatter is not supported; use simple \"key: value\" entries", lineNo)
		}
		if strings.HasPrefix(trimmed, "- ") {
			return nil, "", fmt.Errorf("line %d: frontmatter lists are not supported; use simple \"key: value\" entries", lineNo)
		}
		key, value, found := strings.Cut(trimmed, ":")
		if !found {
			return nil, "", fmt.Errorf("line %d: expected \"key: value\", got %q", lineNo, trimmed)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "" {
			return nil, "", fmt.Errorf("line %d: empty key", lineNo)
		}
		meta[key] = unquote(strings.TrimSpace(value))
	}
	return nil, "", fmt.Errorf("frontmatter block is not closed with ---")
}

// trimFrontmatterOpen removes the opening delimiter, tolerating a UTF-8
// byte-order mark, which Windows editors add.
func trimFrontmatterOpen(content string) (string, bool) {
	content = strings.TrimPrefix(content, "\ufeff")
	for _, open := range []string{"---\n", "---\r\n"} {
		if strings.HasPrefix(content, open) {
			return content[len(open):], true
		}
	}
	return "", false
}

// unquote removes matching surrounding quotes.
func unquote(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// Body reads a skill's instructions.
func (s Skill) Body() (string, error) {
	b, err := os.ReadFile(s.Path)
	if err != nil {
		return "", fmt.Errorf("read skill %q: %w", s.Name, err)
	}
	if len(b) > MaxBodyBytes {
		return "", fmt.Errorf("skill %q is %d bytes, larger than the %d byte limit; "+
			"split it into several skills", s.Name, len(b), MaxBodyBytes)
	}
	_, body, err := splitFrontmatter(string(b))
	if err != nil {
		return "", fmt.Errorf("%s: %w", s.Path, err)
	}
	return strings.TrimSpace(body), nil
}

// SupportingFiles lists the other files in a skill's directory, so the model
// can be told what else it may read.
func (s Skill) SupportingFiles() []string {
	var out []string
	_ = filepath.WalkDir(s.Dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if filepath.Base(p) == FileName {
			return nil
		}
		if rel, err := filepath.Rel(s.Dir, p); err == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}
