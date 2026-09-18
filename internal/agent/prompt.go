package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"mkrcode/internal/config"
	"mkrcode/internal/tools"
)

// memoryFileName is the per-project instruction file loaded into the
// system prompt, the equivalent of a README written for the agent.
const memoryFileName = "MKR.md"

// maxMemoryBytes caps how much of a memory file is loaded, so an oversized
// file cannot crowd out the conversation.
const maxMemoryBytes = 32 << 10

// SystemPrompt builds the instructions sent at the head of every request.
//
// The prompt is assembled rather than stored as a constant because it has
// to describe the current permission mode accurately. A model told it can
// edit files while running in plan mode will waste turns on calls that are
// refused.
func SystemPrompt(cfg config.Config, reg *tools.Registry) string {
	var b strings.Builder

	b.WriteString(`You are mkr, a coding assistant operating inside an air-gapped environment.
You work directly in the operator's workspace using the tools provided.

# Operating principles

- Investigate before acting. Read the relevant files rather than assuming
  what they contain.
- Make the change that was asked for. Do not expand scope, refactor
  unrelated code, or add features nobody requested.
- Prefer edit_file over write_file for existing files, so unrelated content
  is preserved.
- You must read a file before editing it.
- After changing code, run the project's tests or build if a command for
  them exists.
- Report honestly. If a command failed or a test did not pass, say so
  plainly and show the output.
- Keep responses short. The operator is reading in a terminal.
`)

	fmt.Fprintf(&b, "\n# Environment\n\n")
	fmt.Fprintf(&b, "- Workspace: %s\n", cfg.Workspace)
	fmt.Fprintf(&b, "- Operating system: %s\n", osName())
	fmt.Fprintf(&b, "- Shell for the exec tool: %s\n", shellName(cfg.Shell))
	fmt.Fprintf(&b, "- All file paths are relative to the workspace root. "+
		"Paths outside it are refused.\n")
	if cfg.Redact {
		fmt.Fprintf(&b, "- Tool output is scanned for credentials. Text shown as "+
			"[REDACTED:kind] was a secret that has been removed; do not try to recover it.\n")
	}

	b.WriteString("\n# Permission mode\n\n")
	switch cfg.Mode {
	case config.ModePlan:
		b.WriteString(`You are in PLAN mode. This is a read-only session.

write_file, edit_file and exec WILL BE REFUSED. Do not call them.

Investigate using read_file, list_dir, glob and grep, then present a concrete
plan: which files you would change, what the change is, and how it would be
verified. The operator will switch you to another mode if they want it done.
`)
	case config.ModeAuto:
		b.WriteString(`You are in AUTO mode. File edits and commands run without
asking the operator first. Be careful and deliberate: verify before you
change, and prefer the smallest edit that does the job. Some commands are
refused by policy regardless of mode; if one is refused, do not attempt to
work around it, and tell the operator instead.
`)
	default:
		b.WriteString(`You are in APPROVE mode. The operator is asked before every
file write and every command. Make each proposed action easy to approve: one
clear change at a time, with a short explanation of why. If an action is
declined, do not retry it; propose an alternative or ask what they would
prefer.
`)
	}

	if names := reg.Names(); len(names) > 0 {
		fmt.Fprintf(&b, "\n# Tools available\n\n%s\n", strings.Join(names, ", "))
	}

	if mem := loadMemory(cfg.Workspace); mem != "" {
		b.WriteString("\n# Project instructions\n\n")
		b.WriteString("The following came from the workspace's " + memoryFileName +
			". Treat it as instructions from the operator.\n\n")
		b.WriteString(mem)
		b.WriteString("\n")
	}
	return b.String()
}

// loadMemory reads the user-global and project memory files, project last
// so it takes precedence in the model's reading.
func loadMemory(workspace string) string {
	var parts []string

	if dir, err := config.UserConfigDir(); err == nil {
		if s := readCapped(filepath.Join(dir, memoryFileName)); s != "" {
			parts = append(parts, "## Operator preferences\n\n"+s)
		}
	}
	if workspace != "" {
		if s := readCapped(filepath.Join(workspace, memoryFileName)); s != "" {
			parts = append(parts, "## This project\n\n"+s)
		}
	}
	return strings.Join(parts, "\n\n")
}

// readCapped reads a file up to maxMemoryBytes, returning "" if absent.
func readCapped(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(b) > maxMemoryBytes {
		b = append(b[:maxMemoryBytes], []byte("\n\n[truncated]")...)
	}
	return strings.TrimSpace(string(b))
}

// osName renders the host platform for the prompt.
func osName() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	default:
		return runtime.GOOS
	}
}

// shellName renders the exec shell, describing its syntax so the model does
// not emit bash into PowerShell.
func shellName(shell string) string {
	base := strings.ToLower(filepath.Base(shell))
	base = strings.TrimSuffix(base, ".exe")
	switch base {
	case "pwsh":
		return "PowerShell 7 (pwsh). Use PowerShell syntax, not bash."
	case "powershell":
		return "Windows PowerShell 5.1. Use PowerShell syntax, not bash."
	case "cmd":
		return "cmd.exe. Use Windows batch syntax."
	case "":
		return "the system shell"
	default:
		return base
	}
}
