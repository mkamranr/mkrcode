// Package tools implements the actions the agent can take: reading and
// writing files, searching the workspace, and running shell commands.
//
// Every tool that touches the filesystem routes its paths through
// internal/fsjail, and every tool declares whether it mutates state so the
// permission layer can gate it without knowing what the tool does.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"mkrcode/internal/fsjail"
)

// Result is the outcome of a tool invocation.
type Result struct {
	// Content is the text returned to the model.
	Content string
	// Display, when set, is shown to the operator instead of Content,
	// which may be large.
	Display string
	// Diff is a unified diff of a file mutation, recorded in the audit log.
	Diff string
	// IsError marks a failure that the model should see and recover from,
	// as opposed to a fault that should abort the turn.
	IsError bool
}

// Errorf builds a Result carrying a recoverable error for the model.
func Errorf(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// Tool is one capability exposed to the model.
type Tool interface {
	// Name is the identifier the model calls.
	Name() string
	// Description tells the model when to use the tool.
	Description() string
	// Schema is the JSON Schema of the arguments object.
	Schema() json.RawMessage
	// Mutating reports whether the tool changes state. Mutating tools are
	// refused in plan mode and prompted for in approve mode.
	Mutating() bool
	// Describe renders a one-line summary for the approval prompt and the
	// audit log. It must never fail, so malformed arguments render as-is.
	Describe(args json.RawMessage) string
	// Run executes the tool. A returned error aborts the turn; a Result
	// with IsError set is handed back to the model to recover from.
	Run(ctx context.Context, args json.RawMessage) (Result, error)
}

// Registry holds the tools available in a session.
type Registry struct {
	byName map[string]Tool
	order  []string
}

// NewRegistry returns a registry containing the given tools.
func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		r.Add(t)
	}
	return r
}

// Add registers a tool, replacing any earlier tool of the same name.
func (r *Registry) Add(t Tool) {
	if _, exists := r.byName[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.byName[t.Name()] = t
}

// Get returns the named tool.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// All returns every tool in registration order.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.byName[n])
	}
	return out
}

// Names returns the registered tool names, sorted.
func (r *Registry) Names() []string {
	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}

// Options configures the standard tool set.
type Options struct {
	// Jail confines every filesystem path.
	Jail *fsjail.Jail
	// Shell is the executable used by the exec tool.
	Shell string
	// ExecTimeout bounds a single command.
	ExecTimeoutSeconds int
	// MaxOutputBytes caps what any one tool returns to the model, so a
	// single large file cannot consume the whole context window.
	MaxOutputBytes int
	// Tracker records which files have been read, enforcing read-before-edit.
	Tracker *ReadTracker
}

// DefaultMaxOutputBytes is the cap applied when Options leaves it unset.
const DefaultMaxOutputBytes = 256 << 10 // 256 KiB

// Standard returns the tool set available in v1.
func Standard(opt Options) *Registry {
	if opt.MaxOutputBytes <= 0 {
		opt.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if opt.Tracker == nil {
		opt.Tracker = NewReadTracker()
	}
	return NewRegistry(
		&readFile{opt: opt},
		&writeFile{opt: opt},
		&editFile{opt: opt},
		&listDir{opt: opt},
		&globTool{opt: opt},
		&grepTool{opt: opt},
		&execTool{opt: opt},
	)
}

// decode unmarshals tool arguments, producing an error the model can act on.
func decode(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(strings.NewReader(string(args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// Retry permissively: an unexpected extra field is a model slip,
		// not a reason to fail the call outright.
		if err2 := json.Unmarshal(args, dst); err2 == nil {
			return nil
		}
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// truncate caps s at n bytes, appending a notice the model can understand.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	// Cut at a rune boundary so the result stays valid UTF-8.
	cut := n
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n[output truncated: %d of %d bytes shown]", cut, len(s)), true
}

// utf8Start reports whether b begins a UTF-8 rune.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }
