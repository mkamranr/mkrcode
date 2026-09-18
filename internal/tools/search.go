package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipDirs are never descended into. Searching them wastes the context
// window on vendored and generated content.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
	"venv": true, "__pycache__": true, "dist": true, "build": true,
	"target": true, ".idea": true, ".vscode": true, ".mkr": true,
	"bin": true, "obj": true, ".gradle": true, ".terraform": true,
}

// listDir lists the entries of a directory.
type listDir struct{ opt Options }

func (t *listDir) Name() string { return "list_dir" }

func (t *listDir) Description() string {
	return "List the files and directories at a path in the workspace. " +
		"Use this to explore the project structure."
}

func (t *listDir) Mutating() bool { return false }

func (t *listDir) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory relative to the workspace root. Defaults to the root."}
  }
}`)
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (t *listDir) Describe(args json.RawMessage) string {
	var a listDirArgs
	decode(args, &a)
	if a.Path == "" {
		a.Path = "."
	}
	return "list " + a.Path
}

func (t *listDir) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a listDirArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if a.Path == "" {
		a.Path = "."
	}
	abs, err := t.opt.Jail.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return Errorf("directory not found: %s", a.Path), nil
		}
		return Errorf("cannot list %s: %v", a.Path, err), nil
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})

	var b strings.Builder
	fmt.Fprintf(&b, "%s/\n", t.opt.Jail.Rel(abs))
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(&b, "  %s/\n", e.Name())
			continue
		}
		info, err := e.Info()
		if err != nil {
			fmt.Fprintf(&b, "  %s\n", e.Name())
			continue
		}
		fmt.Fprintf(&b, "  %s (%s)\n", e.Name(), humanSize(info.Size()))
	}
	if len(entries) == 0 {
		b.WriteString("  (empty)\n")
	}

	out, _ := truncate(b.String(), t.opt.MaxOutputBytes)
	return Result{Content: out, Display: fmt.Sprintf("listed %s (%d entries)", t.opt.Jail.Rel(abs), len(entries))}, nil
}

// globTool finds files by name pattern.
type globTool struct{ opt Options }

func (t *globTool) Name() string { return "glob" }

func (t *globTool) Description() string {
	return "Find files whose path matches a glob pattern, for example **/*.go or src/**/*_test.py. " +
		"Returns paths relative to the workspace root."
}

func (t *globTool) Mutating() bool { return false }

func (t *globTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Glob pattern. ** matches across directories."},
    "path": {"type": "string", "description": "Directory to search under. Defaults to the workspace root."}
  },
  "required": ["pattern"]
}`)
}

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (t *globTool) Describe(args json.RawMessage) string {
	var a globArgs
	decode(args, &a)
	return "glob " + a.Pattern
}

// maxMatches bounds every search result set.
const maxMatches = 500

func (t *globTool) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a globArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Errorf("pattern is required"), nil
	}
	root := a.Path
	if root == "" {
		root = "."
	}
	absRoot, err := t.opt.Jail.Resolve(root)
	if err != nil {
		return Errorf("%v", err), nil
	}

	re, err := globToRegexp(a.Pattern)
	if err != nil {
		return Errorf("invalid pattern %q: %v", a.Pattern, err), nil
	}

	var matches []string
	err = filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != absRoot {
				return filepath.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(relTo(absRoot, p))
		if re.MatchString(rel) {
			matches = append(matches, filepath.ToSlash(t.opt.Jail.Rel(p)))
		}
		if len(matches) >= maxMatches {
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	sort.Strings(matches)
	if len(matches) == 0 {
		return Result{Content: fmt.Sprintf("no files matched %q", a.Pattern), Display: "glob: no matches"}, nil
	}
	body := strings.Join(matches, "\n")
	if len(matches) >= maxMatches {
		body += fmt.Sprintf("\n\n[stopped at %d matches; narrow the pattern]", maxMatches)
	}
	out, _ := truncate(body, t.opt.MaxOutputBytes)
	return Result{Content: out, Display: fmt.Sprintf("glob %s (%d matches)", a.Pattern, len(matches))}, nil
}

// grepTool searches file contents by regular expression.
type grepTool struct{ opt Options }

func (t *grepTool) Name() string { return "grep" }

func (t *grepTool) Description() string {
	return "Search file contents with a regular expression (Go/RE2 syntax). " +
		"Returns matching lines with their file and line number."
}

func (t *grepTool) Mutating() bool { return false }

func (t *grepTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern": {"type": "string", "description": "Regular expression to search for."},
    "path": {"type": "string", "description": "Directory to search under. Defaults to the workspace root."},
    "glob": {"type": "string", "description": "Only search files matching this glob, for example **/*.go."},
    "ignore_case": {"type": "boolean", "description": "Match case-insensitively."},
    "context": {"type": "integer", "description": "Lines of context to show around each match."}
  },
  "required": ["pattern"]
}`)
}

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignore_case"`
	Context    int    `json:"context"`
}

func (t *grepTool) Describe(args json.RawMessage) string {
	var a grepArgs
	decode(args, &a)
	if a.Glob != "" {
		return fmt.Sprintf("grep %q in %s", a.Pattern, a.Glob)
	}
	return fmt.Sprintf("grep %q", a.Pattern)
}

// maxGrepFileBytes skips files too large to be worth searching inline.
const maxGrepFileBytes = 4 << 20

func (t *grepTool) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a grepArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return Errorf("pattern is required"), nil
	}

	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return Errorf("invalid regular expression %q: %v", a.Pattern, err), nil
	}

	var nameRe *regexp.Regexp
	if a.Glob != "" {
		nameRe, err = globToRegexp(a.Glob)
		if err != nil {
			return Errorf("invalid glob %q: %v", a.Glob, err), nil
		}
	}

	root := a.Path
	if root == "" {
		root = "."
	}
	absRoot, err := t.opt.Jail.Resolve(root)
	if err != nil {
		return Errorf("%v", err), nil
	}

	var (
		b       strings.Builder
		hits    int
		files   int
		stopped bool
	)
	walkErr := filepath.WalkDir(absRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != absRoot {
				return filepath.SkipDir
			}
			return nil
		}
		rel := filepath.ToSlash(relTo(absRoot, p))
		if nameRe != nil && !nameRe.MatchString(rel) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxGrepFileBytes {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil || isBinary(raw) {
			return nil
		}

		lines := splitLines(string(raw))
		matched := false
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			if !matched {
				files++
				matched = true
			}
			display := filepath.ToSlash(t.opt.Jail.Rel(p))
			lo, hi := i-a.Context, i+a.Context
			if lo < 0 {
				lo = 0
			}
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			for k := lo; k <= hi; k++ {
				sep := ":"
				if k != i {
					sep = "-"
				}
				fmt.Fprintf(&b, "%s%s%d%s%s\n", display, sep, k+1, sep, lines[k])
			}
			if a.Context > 0 {
				b.WriteString("--\n")
			}
			hits++
			if hits >= maxMatches {
				stopped = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil && ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	if hits == 0 {
		return Result{
			Content: fmt.Sprintf("no matches for %q", a.Pattern),
			Display: "grep: no matches",
		}, nil
	}
	body := b.String()
	if stopped {
		body += fmt.Sprintf("\n[stopped at %d matches; narrow the search]\n", maxMatches)
	}
	out, _ := truncate(body, t.opt.MaxOutputBytes)
	return Result{
		Content: out,
		Display: fmt.Sprintf("grep %q (%d matches in %d files)", a.Pattern, hits, files),
	}, nil
}

// relTo returns p relative to base, falling back to p.
func relTo(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return p
	}
	return rel
}

// globToRegexp translates a glob into an anchored regular expression.
//
// Supported: * within a path segment, ** across segments, ? for one
// character, and [...] character classes. A pattern with no slash matches
// against the basename as well, so "*.go" behaves as people expect.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	p := filepath.ToSlash(strings.TrimPrefix(pattern, "./"))
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				i++
				// "**/" may match zero directories, so the slash is optional.
				if i+1 < len(p) && p[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(p[i:], ']')
			if end < 0 {
				b.WriteString(regexp.QuoteMeta(string(c)))
				continue
			}
			class := p[i : i+end+1]
			b.WriteString(class)
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil, err
	}
	if !strings.Contains(p, "/") {
		// Also match the basename anywhere in the tree.
		alt, err2 := regexp.Compile("(?:^|/)" + strings.TrimPrefix(b.String(), "^"))
		if err2 == nil {
			return alt, nil
		}
	}
	return re, nil
}

// humanSize renders a byte count compactly.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(n)/float64(div), "KMGT"[exp])
}
