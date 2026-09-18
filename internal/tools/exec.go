package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// execTool runs a shell command inside the workspace.
type execTool struct{ opt Options }

func (t *execTool) Name() string { return "exec" }

func (t *execTool) Description() string {
	shell := "PowerShell"
	if runtime.GOOS != "windows" {
		shell = "the shell"
	}
	return "Run a command in " + shell + " from the workspace directory. " +
		"Returns stdout, stderr and the exit code. " +
		"Use this for builds, tests and version control. " +
		"Do not use it to read or write files; use read_file, write_file and edit_file instead."
}

// Mutating reports true unconditionally. A command's effect cannot be known
// before it runs, so exec is always gated by the permission layer.
func (t *execTool) Mutating() bool { return true }

func (t *execTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The command line to run."},
    "timeout_seconds": {"type": "integer", "description": "Maximum seconds to allow. Defaults to the configured limit."},
    "cwd": {"type": "string", "description": "Working directory relative to the workspace root."}
  },
  "required": ["command"]
}`)
}

type execArgs struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	Cwd            string `json:"cwd"`
}

func (t *execTool) Describe(args json.RawMessage) string {
	var a execArgs
	if err := decode(args, &a); err != nil {
		return "exec " + string(args)
	}
	cmd := strings.TrimSpace(a.Command)
	// The approval prompt must show the whole command, but a pathological
	// one-liner should not flood the terminal.
	if len(cmd) > 400 {
		cmd = cmd[:400] + " ..."
	}
	return cmd
}

// maxExecOutput bounds captured output per stream.
const maxExecOutput = 128 << 10

func (t *execTool) Run(ctx context.Context, args json.RawMessage) (Result, error) {
	var a execArgs
	if err := decode(args, &a); err != nil {
		return Errorf("%v", err), nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return Errorf("command is required"), nil
	}

	dir := t.opt.Jail.Root()
	if a.Cwd != "" {
		abs, err := t.opt.Jail.Resolve(a.Cwd)
		if err != nil {
			return Errorf("%v", err), nil
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return Errorf("cwd %q is not a directory in the workspace", a.Cwd), nil
		}
		dir = abs
	}

	timeout := time.Duration(t.opt.ExecTimeoutSeconds) * time.Second
	if a.TimeoutSeconds > 0 {
		timeout = time.Duration(a.TimeoutSeconds) * time.Second
	}
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, shellArgs := shellCommand(t.opt.Shell, a.Command)
	cmd := exec.CommandContext(runCtx, shell, shellArgs...)
	cmd.Dir = dir
	cmd.Env = sanitisedEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Closing stdin means an interactive prompt fails fast instead of
	// hanging until the timeout, which is the common failure mode for an
	// agent running a command that expects input.
	cmd.Stdin = nil

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	exitCode := 0
	timedOut := errors.Is(runCtx.Err(), context.DeadlineExceeded)
	if runErr != nil {
		var ee *exec.ExitError
		switch {
		case errors.As(runErr, &ee):
			exitCode = ee.ExitCode()
		case timedOut:
			exitCode = -1
		default:
			// The shell itself could not be started. On Windows this is
			// almost always pwsh not being installed.
			return Errorf("cannot run %s: %v\n"+
				"Set \"shell\" in config.json to an available shell, or install PowerShell 7.", shell, runErr), nil
		}
	}
	// A cancelled parent context is the operator interrupting, which must
	// propagate rather than being reported as a command failure.
	if ctx.Err() != nil {
		return Result{}, ctx.Err()
	}

	var b strings.Builder
	if outStr := strings.TrimRight(stdout.String(), "\n"); outStr != "" {
		s, _ := truncate(outStr, maxExecOutput)
		b.WriteString(s)
		b.WriteString("\n")
	}
	if errStr := strings.TrimRight(stderr.String(), "\n"); errStr != "" {
		s, _ := truncate(errStr, maxExecOutput)
		b.WriteString("[stderr]\n")
		b.WriteString(s)
		b.WriteString("\n")
	}
	if timedOut {
		fmt.Fprintf(&b, "\n[command timed out after %s]\n", timeout)
	}
	fmt.Fprintf(&b, "[exit code %d]", exitCode)

	if b.Len() == 0 {
		b.WriteString("[no output]")
	}

	display := fmt.Sprintf("exec (exit %d, %s)", exitCode, elapsed.Round(time.Millisecond))
	if timedOut {
		display = fmt.Sprintf("exec (timed out after %s)", timeout)
	}
	return Result{
		Content: b.String(),
		Display: display,
		// A non-zero exit is information for the model, not a tool fault:
		// a failing test run is exactly what it needs to see.
		IsError: false,
	}, nil
}

// shellCommand builds the argv for running line under the given shell.
func shellCommand(shell, line string) (string, []string) {
	if shell == "" {
		shell = DetectShell()
	}
	base := strings.ToLower(shell)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")

	switch base {
	case "pwsh", "powershell":
		// -NoProfile keeps operator profile scripts out of the agent's
		// environment; -NonInteractive makes a prompt fail rather than hang.
		return shell, []string{"-NoProfile", "-NonInteractive", "-Command", line}
	case "cmd":
		return shell, []string{"/C", line}
	default:
		return shell, []string{"-c", line}
	}
}

// DetectShell picks the best available shell for the host.
//
// The Windows fleet standardises on PowerShell 7, but it is not present on
// every machine, so the fallback chain ends somewhere that always exists.
func DetectShell() string {
	var candidates []string
	if runtime.GOOS == "windows" {
		candidates = []string{"pwsh.exe", "powershell.exe", "cmd.exe"}
	} else {
		candidates = []string{"bash", "sh"}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}
	return candidates[len(candidates)-1]
}

// sensitiveEnvSubstrings name environment variables withheld from child
// processes. A command the operator approved still should not inherit
// credentials that happen to be in the session environment.
var sensitiveEnvSubstrings = []string{
	"TOKEN", "SECRET", "PASSWORD", "PASSWD", "APIKEY", "API_KEY",
	"CREDENTIAL", "PRIVATE_KEY", "SESSION_KEY", "ACCESS_KEY",
}

// sanitisedEnv returns the process environment with credential-shaped
// variables removed.
func sanitisedEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		name, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(name)
		// mkr's own configuration includes the endpoint API key.
		if strings.HasPrefix(upper, "MKR_") {
			continue
		}
		sensitive := false
		for _, s := range sensitiveEnvSubstrings {
			if strings.Contains(upper, s) {
				sensitive = true
				break
			}
		}
		if !sensitive {
			out = append(out, kv)
		}
	}
	return out
}
