// Package ui renders the session to the terminal and asks the operator for
// permission decisions.
//
// Everything the agent emits goes through the Renderer interface. The
// streaming stdout implementation here is what v1 ships; keeping the seam
// means a full-screen interface can be added later without touching the
// agent loop.
package ui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"mkrcode/internal/permission"
)

// Renderer receives everything the session displays.
type Renderer interface {
	// Banner is shown once at startup.
	Banner(lines ...string)
	// AssistantText streams a fragment of the model's prose.
	AssistantText(s string)
	// Reasoning streams a fragment of hidden chain-of-thought.
	Reasoning(s string)
	// EndAssistant closes an assistant message.
	EndAssistant()
	// ToolStart announces a tool call about to run.
	ToolStart(tool, summary string)
	// ToolResult reports the outcome of a tool call.
	ToolResult(summary string, isError bool)
	// Info prints an operational notice.
	Info(format string, args ...any)
	// Warn prints a warning.
	Warn(format string, args ...any)
	// Errorf prints an error.
	Errorf(format string, args ...any)
	// Prompt renders the input prompt and returns the operator's line.
	Prompt(mode string) (string, error)
}

// ANSI escape sequences. They are written only when the output is a
// terminal that can interpret them.
const (
	ansiReset  = "\033[0m"
	ansiDim    = "\033[2m"
	ansiBold   = "\033[1m"
	ansiRed    = "\033[31m"
	ansiGreen  = "\033[32m"
	ansiYellow = "\033[33m"
	ansiBlue   = "\033[34m"
	ansiCyan   = "\033[36m"
)

// Terminal renders to a pair of streams.
type Terminal struct {
	mu  sync.Mutex
	out io.Writer
	err io.Writer
	in  *bufio.Reader

	color bool
	// inAssistant tracks whether a streamed message is open, so tool
	// output does not appear glued to the end of a sentence.
	inAssistant bool
	// lastWasReasoning tracks the dim styling currently applied.
	lastWasReasoning bool
}

// NewTerminal returns a Terminal writing to stdout and stderr.
func NewTerminal() *Terminal {
	enableVirtualTerminal()
	return &Terminal{
		out:   os.Stdout,
		err:   os.Stderr,
		in:    bufio.NewReader(os.Stdin),
		color: supportsColor(),
	}
}

// NewTerminalWith returns a Terminal wired to explicit streams, for tests.
func NewTerminalWith(out, errw io.Writer, in io.Reader, color bool) *Terminal {
	return &Terminal{out: out, err: errw, in: bufio.NewReader(in), color: color}
}

// paint wraps s in an escape sequence when colour is enabled.
func (t *Terminal) paint(code, s string) string {
	if !t.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

// Banner implements Renderer.
func (t *Terminal) Banner(lines ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, l := range lines {
		fmt.Fprintln(t.out, t.paint(ansiDim, l))
	}
	fmt.Fprintln(t.out)
}

// AssistantText implements Renderer.
func (t *Terminal) AssistantText(s string) {
	if s == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastWasReasoning {
		fmt.Fprint(t.out, ansiReset)
		t.lastWasReasoning = false
	}
	t.inAssistant = true
	fmt.Fprint(t.out, s)
}

// Reasoning implements Renderer, styling hidden reasoning dimly so it is
// visibly distinct from the answer.
func (t *Terminal) Reasoning(s string) {
	if s == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.lastWasReasoning && t.color {
		fmt.Fprint(t.out, ansiDim)
	}
	t.lastWasReasoning = true
	t.inAssistant = true
	fmt.Fprint(t.out, s)
}

// EndAssistant implements Renderer.
func (t *Terminal) EndAssistant() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastWasReasoning && t.color {
		fmt.Fprint(t.out, ansiReset)
	}
	t.lastWasReasoning = false
	if t.inAssistant {
		fmt.Fprintln(t.out)
		t.inAssistant = false
	}
}

// ToolStart implements Renderer.
func (t *Terminal) ToolStart(tool, summary string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.breakAssistantLocked()
	fmt.Fprintf(t.out, "%s %s\n", t.paint(ansiBlue, "→"), t.paint(ansiBold, summary))
}

// ToolResult implements Renderer.
func (t *Terminal) ToolResult(summary string, isError bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	mark, colour := "✓", ansiGreen
	if isError {
		mark, colour = "✗", ansiRed
	}
	fmt.Fprintf(t.out, "  %s %s\n", t.paint(colour, mark), t.paint(ansiDim, summary))
}

// Info implements Renderer.
func (t *Terminal) Info(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.breakAssistantLocked()
	fmt.Fprintln(t.out, t.paint(ansiCyan, fmt.Sprintf(format, args...)))
}

// Warn implements Renderer.
func (t *Terminal) Warn(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.breakAssistantLocked()
	fmt.Fprintln(t.err, t.paint(ansiYellow, "warning: "+fmt.Sprintf(format, args...)))
}

// Errorf implements Renderer.
func (t *Terminal) Errorf(format string, args ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.breakAssistantLocked()
	fmt.Fprintln(t.err, t.paint(ansiRed, "error: "+fmt.Sprintf(format, args...)))
}

// breakAssistantLocked closes an open streamed message. The caller holds
// the mutex.
func (t *Terminal) breakAssistantLocked() {
	if t.lastWasReasoning && t.color {
		fmt.Fprint(t.out, ansiReset)
		t.lastWasReasoning = false
	}
	if t.inAssistant {
		fmt.Fprintln(t.out)
		t.inAssistant = false
	}
}

// Prompt implements Renderer, returning io.EOF when input closes.
func (t *Terminal) Prompt(mode string) (string, error) {
	t.mu.Lock()
	label := fmt.Sprintf("[%s] › ", mode)
	fmt.Fprint(t.out, t.paint(ansiBold, label))
	t.mu.Unlock()

	line, err := t.in.ReadString('\n')
	if err != nil {
		if err == io.EOF && strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line), nil
		}
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// Confirm implements permission.Prompter.
//
// The operator sees exactly what is about to happen before answering, and
// the default on a bare Enter is to refuse: a permission prompt that is
// easy to accept by accident is not a control.
func (t *Terminal) Confirm(req permission.Request) (bool, bool, error) {
	t.mu.Lock()
	t.breakAssistantLocked()

	fmt.Fprintln(t.out)
	fmt.Fprintf(t.out, "%s %s\n", t.paint(ansiYellow, "permission required:"), t.paint(ansiBold, req.Summary))
	if req.Command != "" {
		fmt.Fprintf(t.out, "  %s %s\n", t.paint(ansiDim, "command:"), req.Command)
	}
	for _, p := range req.Paths {
		fmt.Fprintf(t.out, "  %s %s\n", t.paint(ansiDim, "writes:  "), p)
	}
	fmt.Fprint(t.out, t.paint(ansiBold, "  allow? [y]es / [n]o / [a]lways / [q]uit: "))
	t.mu.Unlock()

	line, err := t.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		// Input closed mid-prompt. Refuse rather than assume consent.
		return false, false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, false, nil
	case "a", "always":
		return true, true, nil
	case "q", "quit":
		return false, false, ErrQuit
	default:
		return false, false, nil
	}
}

// ErrQuit is returned when the operator answers a permission prompt with
// "quit", ending the session rather than just refusing the action.
var ErrQuit = fmt.Errorf("session ended by the operator")

// supportsColor reports whether stdout is a terminal that should be styled.
func supportsColor() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	// A pipe or file is not styled, so redirected output stays clean and
	// the audit trail is not polluted with escape sequences.
	return info.Mode()&os.ModeCharDevice != 0
}
