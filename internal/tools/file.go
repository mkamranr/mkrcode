package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ReadTracker records which files the agent has read, and with what
// content, so edits can be refused when the agent has not seen the current
// file. Editing a file blind is the most common way an agent destroys work.
type ReadTracker struct {
	mu   sync.Mutex
	seen map[string]string // absolute path -> content when last read
}

// NewReadTracker returns an empty tracker.
func NewReadTracker() *ReadTracker {
	return &ReadTracker{seen: map[string]string{}}
}

// MarkRead records the content observed at path.
func (t *ReadTracker) MarkRead(path, content string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen[path] = content
}

// HasRead reports whether path has been read in this session.
func (t *ReadTracker) HasRead(path string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.seen[path]
	return ok
}

// Forget drops the record for path, used after a write replaces it.
func (t *ReadTracker) Forget(path string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, path)
}

// maxReadBytes bounds a single file read regardless of the output cap, so
// a huge file produces a clear error rather than a slow truncation.
const maxReadBytes = 16 << 20 // 16 MiB

// readFile returns the contents of a file with line numbers.
type readFile struct{ opt Options }

func (t *readFile) Name() string { return "read_file" }

func (t *readFile) Description() string {
	return "Read a file from the workspace. Returns the contents with line numbers. " +
		"Use offset and limit to page through a large file. " +
		"You must read a file before editing it."
}

func (t *readFile) Mutating() bool { return false }

func (t *readFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path relative to the workspace root."},
    "offset": {"type": "integer", "description": "First line to return, 1-based. Default 1."},
    "limit": {"type": "integer", "description": "Maximum number of lines to return. Default 2000."}
  },
  "required": ["path"]
}`)
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (t *readFile) Describe(args json.RawMessage) string {
	var a readFileArgs
	if err := decode(args, &a); err != nil {
		return "read_file " + string(args)
	}
	if a.Offset > 0 || a.Limit > 0 {
		return fmt.Sprintf("read %s (from line %d)", a.Path, max(a.Offset, 1))
	}
	return "read " + a.Path
}

// defaultReadLines bounds an unpaged read.
const defaultReadLines = 2000

func (t *readFile) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a readFileArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	abs, err := t.opt.Jail.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return Errorf("file not found: %s", a.Path), nil
		}
		return Errorf("cannot read %s: %v", a.Path, err), nil
	}
	if info.IsDir() {
		return Errorf("%s is a directory; use list_dir", a.Path), nil
	}
	if info.Size() > maxReadBytes {
		return Errorf("%s is %d bytes, larger than the %d byte read limit", a.Path, info.Size(), maxReadBytes), nil
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return Errorf("cannot read %s: %v", a.Path, err), nil
	}
	if isBinary(raw) {
		return Errorf("%s appears to be a binary file (%d bytes)", a.Path, len(raw)), nil
	}

	content := string(raw)
	t.opt.Tracker.MarkRead(abs, content)

	offset := a.Offset
	if offset <= 0 {
		offset = 1
	}
	limit := a.Limit
	if limit <= 0 {
		limit = defaultReadLines
	}

	lines := strings.Split(content, "\n")
	// A trailing newline produces a final empty element that is not a line.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if offset > len(lines) {
		return Errorf("offset %d is past the end of %s, which has %d lines", offset, a.Path, len(lines)), nil
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}

	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "\n[showing lines %d-%d of %d; call again with offset=%d for more]\n", offset, end, len(lines), end+1)
	}

	out, _ := truncate(b.String(), t.opt.MaxOutputBytes)
	return Result{
		Content: out,
		Display: fmt.Sprintf("read %s (%d lines)", t.opt.Jail.Rel(abs), end-offset+1),
	}, nil
}

// writeFile creates or replaces a file.
type writeFile struct{ opt Options }

func (t *writeFile) Name() string { return "write_file" }

func (t *writeFile) Description() string {
	return "Write a complete file to the workspace, creating it or replacing its entire contents. " +
		"To change part of an existing file, prefer edit_file."
}

func (t *writeFile) Mutating() bool { return true }

func (t *writeFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path relative to the workspace root."},
    "content": {"type": "string", "description": "The complete new contents of the file."}
  },
  "required": ["path", "content"]
}`)
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *writeFile) Describe(args json.RawMessage) string {
	var a writeFileArgs
	if err := decode(args, &a); err != nil {
		return "write_file " + string(args)
	}
	return fmt.Sprintf("write %s (%d bytes)", a.Path, len(a.Content))
}

func (t *writeFile) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a writeFileArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	abs, err := t.opt.Jail.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}

	before := ""
	existed := false
	if b, err := os.ReadFile(abs); err == nil {
		before, existed = string(b), true
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return Errorf("cannot create directory for %s: %v", a.Path, err), nil
	}
	if err := os.WriteFile(abs, []byte(a.Content), 0o644); err != nil {
		return Errorf("cannot write %s: %v", a.Path, err), nil
	}

	// The file on disk is now what the agent just wrote, so an immediate
	// edit is safe without a re-read.
	t.opt.Tracker.MarkRead(abs, a.Content)

	rel := t.opt.Jail.Rel(abs)
	verb := "created"
	if existed {
		verb = "updated"
	}
	return Result{
		Content: fmt.Sprintf("%s %s (%d bytes)", verb, rel, len(a.Content)),
		Display: fmt.Sprintf("%s %s", verb, rel),
		Diff:    UnifiedDiff(rel, before, a.Content),
	}, nil
}

// editFile replaces an exact string within a file.
type editFile struct{ opt Options }

func (t *editFile) Name() string { return "edit_file" }

func (t *editFile) Description() string {
	return "Replace an exact string in a file. The old_string must appear exactly once " +
		"unless replace_all is true, and must match the file byte for byte including " +
		"indentation. Read the file first."
}

func (t *editFile) Mutating() bool { return true }

func (t *editFile) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Path relative to the workspace root."},
    "old_string": {"type": "string", "description": "Exact text to replace, including indentation."},
    "new_string": {"type": "string", "description": "Replacement text."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring exactly one."}
  },
  "required": ["path", "old_string", "new_string"]
}`)
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *editFile) Describe(args json.RawMessage) string {
	var a editFileArgs
	if err := decode(args, &a); err != nil {
		return "edit_file " + string(args)
	}
	if a.ReplaceAll {
		return fmt.Sprintf("edit %s (replace all occurrences)", a.Path)
	}
	return "edit " + a.Path
}

func (t *editFile) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a editFileArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	abs, err := t.opt.Jail.Resolve(a.Path)
	if err != nil {
		return Errorf("%v", err), nil
	}
	if a.OldString == a.NewString {
		return Errorf("old_string and new_string are identical; nothing to do"), nil
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return Errorf("file not found: %s", a.Path), nil
		}
		return Errorf("cannot read %s: %v", a.Path, err), nil
	}
	before := string(raw)

	// Editing a file the agent has not read is how unseen work gets
	// destroyed, so it is refused rather than merely discouraged.
	if !t.opt.Tracker.HasRead(abs) {
		return Errorf("read %s before editing it", a.Path), nil
	}

	n := strings.Count(before, a.OldString)
	switch {
	case n == 0:
		return Errorf("old_string was not found in %s; it must match the file exactly, including whitespace", a.Path), nil
	case n > 1 && !a.ReplaceAll:
		return Errorf("old_string appears %d times in %s; include more surrounding context to make it unique, or set replace_all", n, a.Path), nil
	}

	after := before
	if a.ReplaceAll {
		after = strings.ReplaceAll(before, a.OldString, a.NewString)
	} else {
		after = strings.Replace(before, a.OldString, a.NewString, 1)
	}

	if err := os.WriteFile(abs, []byte(after), 0o644); err != nil {
		return Errorf("cannot write %s: %v", a.Path, err), nil
	}
	t.opt.Tracker.MarkRead(abs, after)

	rel := t.opt.Jail.Rel(abs)
	replaced := 1
	if a.ReplaceAll {
		replaced = n
	}
	return Result{
		Content: fmt.Sprintf("edited %s (%d replacement(s))", rel, replaced),
		Display: fmt.Sprintf("edited %s", rel),
		Diff:    UnifiedDiff(rel, before, after),
	}, nil
}

// isBinary reports whether b looks like binary data. A NUL byte in the
// first few KiB is the same heuristic diff and grep use.
func isBinary(b []byte) bool {
	n := len(b)
	if n > 8000 {
		n = 8000
	}
	for i := 0; i < n; i++ {
		if b[i] == 0 {
			return true
		}
	}
	return false
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
