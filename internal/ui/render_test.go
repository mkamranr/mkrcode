package ui

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"mkrcode/internal/permission"
)

func newTestTerminal(input string) (*Terminal, *bytes.Buffer, *bytes.Buffer) {
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	return NewTerminalWith(out, errw, strings.NewReader(input), false), out, errw
}

// The permission prompt is a security control. These cases pin exactly which
// inputs mean yes, because a mis-parsed answer silently grants an action the
// operator refused.
func TestConfirmAnswerParsing(t *testing.T) {
	req := permission.Request{Tool: "write_file", Summary: "write a.go", Paths: []string{"a.go"}}

	tests := []struct {
		name         string
		input        string
		wantAllow    bool
		wantRemember bool
		wantQuit     bool
	}{
		{"yes", "y\n", true, false, false},
		{"yes spelled out", "yes\n", true, false, false},
		{"yes uppercase", "Y\n", true, false, false},
		{"yes with spaces", "  y  \n", true, false, false},
		{"always", "a\n", true, true, false},
		{"always spelled out", "always\n", true, true, false},
		{"no", "n\n", false, false, false},
		{"no spelled out", "no\n", false, false, false},
		{"quit", "q\n", false, false, true},

		// Everything below must refuse. A permission prompt that treats an
		// ambiguous answer as consent is not a control.
		{"bare enter refuses", "\n", false, false, false},
		{"whitespace refuses", "   \n", false, false, false},
		{"garbage refuses", "maybe\n", false, false, false},
		{"yolo refuses", "yolo\n", false, false, false},
		{"closed stdin refuses", "", false, false, false},
		{"eof mid answer refuses", "y", true, false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term, _, _ := newTestTerminal(tt.input)
			allow, remember, err := term.Confirm(req)

			if tt.wantQuit {
				if err != ErrQuit {
					t.Errorf("err = %v, want ErrQuit", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if allow != tt.wantAllow {
				t.Errorf("allow = %t, want %t for input %q", allow, tt.wantAllow, tt.input)
			}
			if remember != tt.wantRemember {
				t.Errorf("remember = %t, want %t for input %q", remember, tt.wantRemember, tt.input)
			}
		})
	}
}

// The operator must see exactly what they are approving before answering.
func TestConfirmShowsWhatIsBeingApproved(t *testing.T) {
	term, out, _ := newTestTerminal("n\n")
	term.Confirm(permission.Request{
		Tool:    "exec",
		Summary: "run the test suite",
		Command: "go test ./... -run TestSecret",
		Paths:   []string{"src/a.go", "src/b.go"},
	})

	s := out.String()
	for _, want := range []string{
		"permission required",
		"run the test suite",
		"go test ./... -run TestSecret",
		"src/a.go",
		"src/b.go",
		"[y]es",
		"[n]o",
		"[a]lways",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("prompt is missing %q:\n%s", want, s)
		}
	}
}

func TestAssistantTextStreams(t *testing.T) {
	term, out, _ := newTestTerminal("")
	term.AssistantText("Hello, ")
	term.AssistantText("world.")
	term.EndAssistant()

	if got := out.String(); got != "Hello, world."+"\n" {
		t.Errorf("out = %q, want the fragments joined and a trailing newline", got)
	}
}

// EndAssistant on a message that was never opened must not emit a stray
// blank line before every prompt.
func TestEndAssistantWithoutTextIsQuiet(t *testing.T) {
	term, out, _ := newTestTerminal("")
	term.EndAssistant()
	term.EndAssistant()
	if got := out.String(); got != "" {
		t.Errorf("out = %q, want nothing", got)
	}
}

// Tool output must not be glued onto the end of a streamed sentence.
func TestToolStartBreaksTheAssistantLine(t *testing.T) {
	term, out, _ := newTestTerminal("")
	term.AssistantText("Let me look.")
	term.ToolStart("read_file", "read a.go")

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), out.String())
	}
	if lines[0] != "Let me look." {
		t.Errorf("first line = %q", lines[0])
	}
	if !strings.Contains(lines[1], "read a.go") {
		t.Errorf("second line = %q, want the tool summary", lines[1])
	}
}

func TestToolResultMarksErrors(t *testing.T) {
	term, out, _ := newTestTerminal("")
	term.ToolResult("read a.go (42 lines)", false)
	term.ToolResult("file not found", true)

	s := out.String()
	if !strings.Contains(s, "✓") || !strings.Contains(s, "read a.go (42 lines)") {
		t.Errorf("success not rendered:\n%s", s)
	}
	if !strings.Contains(s, "✗") || !strings.Contains(s, "file not found") {
		t.Errorf("failure not rendered:\n%s", s)
	}
}

// Warnings and errors belong on stderr so redirected output stays clean.
func TestWarningsAndErrorsGoToStderr(t *testing.T) {
	term, out, errw := newTestTerminal("")
	term.Warn("disk is nearly full")
	term.Errorf("endpoint unreachable")

	if out.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", out.String())
	}
	s := errw.String()
	if !strings.Contains(s, "disk is nearly full") || !strings.Contains(s, "endpoint unreachable") {
		t.Errorf("stderr missing content:\n%s", s)
	}
}

// With colour off, output must contain no escape sequences, so a redirected
// session or a captured log stays readable.
func TestNoColourProducesCleanOutput(t *testing.T) {
	term, out, errw := newTestTerminal("n\n")
	term.Banner("mkr 1.0", "workspace /ws")
	term.AssistantText("text")
	term.Reasoning("thinking")
	term.EndAssistant()
	term.ToolStart("exec", "go test")
	term.ToolResult("ok", false)
	term.Info("info")
	term.Warn("warn")
	term.Errorf("err")
	term.Confirm(permission.Request{Tool: "t", Summary: "s"})

	for name, buf := range map[string]*bytes.Buffer{"stdout": out, "stderr": errw} {
		if strings.Contains(buf.String(), "\033[") {
			t.Errorf("%s contains ANSI escapes with colour disabled:\n%q", name, buf.String())
		}
	}
}

func TestColourIsAppliedWhenEnabled(t *testing.T) {
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	term := NewTerminalWith(out, errw, strings.NewReader(""), true)
	term.ToolResult("done", false)
	if !strings.Contains(out.String(), "\033[") {
		t.Errorf("expected ANSI escapes with colour enabled, got %q", out.String())
	}
}

func TestPromptReturnsTrimmedLine(t *testing.T) {
	term, out, _ := newTestTerminal("  hello world  \n")
	line, err := term.Prompt("approve")
	if err != nil {
		t.Fatal(err)
	}
	if line != "hello world" {
		t.Errorf("line = %q, want it trimmed", line)
	}
	if !strings.Contains(out.String(), "approve") {
		t.Errorf("the prompt should show the current mode, got %q", out.String())
	}
}

// Ctrl+D at an empty prompt ends the session; that is how the REPL exits.
func TestPromptReturnsEOFOnClosedInput(t *testing.T) {
	term, _, _ := newTestTerminal("")
	if _, err := term.Prompt("approve"); err != io.EOF {
		t.Errorf("err = %v, want io.EOF", err)
	}
}

// A final line without a newline is still a real answer, not an EOF.
func TestPromptAcceptsUnterminatedFinalLine(t *testing.T) {
	term, _, _ := newTestTerminal("do the thing")
	line, err := term.Prompt("approve")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if line != "do the thing" {
		t.Errorf("line = %q", line)
	}
}

// Reasoning is styled differently from the answer, and the style must be
// reset so it does not bleed into subsequent output.
func TestReasoningStyleIsResetBeforePlainText(t *testing.T) {
	out, errw := &bytes.Buffer{}, &bytes.Buffer{}
	term := NewTerminalWith(out, errw, strings.NewReader(""), true)
	term.Reasoning("pondering")
	term.AssistantText("answer")
	term.EndAssistant()

	s := out.String()
	dim := strings.Index(s, ansiDim)
	reset := strings.Index(s, ansiReset)
	answer := strings.Index(s, "answer")
	if dim < 0 || reset < 0 {
		t.Fatalf("expected dim and reset sequences, got %q", s)
	}
	if !(dim < reset && reset < answer) {
		t.Errorf("the dim style was not reset before the answer: %q", s)
	}
}
