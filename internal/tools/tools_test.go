package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mkamranr/mkrcode/internal/fsjail"
)

func newTestTools(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	j, err := fsjail.New(root)
	if err != nil {
		t.Fatalf("fsjail: %v", err)
	}
	r := Standard(Options{
		Jail:               j,
		Shell:              DetectShell(),
		ExecTimeoutSeconds: 20,
		MaxOutputBytes:     64 << 10,
	})
	return r, root
}

func run(t *testing.T, r *Registry, name string, args any) Result {
	t.Helper()
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	res, err := tool.Run(context.Background(), b)
	if err != nil {
		t.Fatalf("%s returned a fatal error: %v", name, err)
	}
	return res
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStandardRegistryContents(t *testing.T) {
	r, _ := newTestTools(t)
	want := []string{"edit_file", "exec", "glob", "grep", "list_dir", "read_file", "write_file"}
	got := r.Names()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools = %v, want %v", got, want)
	}
	// Exactly the state-changing tools must declare themselves mutating.
	mutating := map[string]bool{"write_file": true, "edit_file": true, "exec": true}
	for _, tool := range r.All() {
		if tool.Mutating() != mutating[tool.Name()] {
			t.Errorf("%s.Mutating() = %t, want %t", tool.Name(), tool.Mutating(), mutating[tool.Name()])
		}
		if !json.Valid(tool.Schema()) {
			t.Errorf("%s has an invalid JSON schema", tool.Name())
		}
		if tool.Description() == "" {
			t.Errorf("%s has no description", tool.Name())
		}
	}
}

func TestReadFile(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")

	res := run(t, r, "read_file", map[string]any{"path": "a.txt"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, want := range []string{"1\tone", "2\ttwo", "3\tthree"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("output missing %q:\n%s", want, res.Content)
		}
	}
}

func TestReadFilePaging(t *testing.T) {
	r, root := newTestTools(t)
	var sb strings.Builder
	for i := 1; i <= 100; i++ {
		sb.WriteString("line\n")
	}
	write(t, root, "big.txt", sb.String())

	res := run(t, r, "read_file", map[string]any{"path": "big.txt", "offset": 10, "limit": 5})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "    10\tline") {
		t.Errorf("expected line 10, got:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "    15\tline") {
		t.Errorf("limit not honoured, got:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "offset=15") {
		t.Errorf("expected a continuation hint, got:\n%s", res.Content)
	}
}

func TestReadFileErrors(t *testing.T) {
	r, root := newTestTools(t)
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	write(t, root, "bin.dat", "ok\x00binary")

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"missing", map[string]any{"path": "nope.txt"}, "not found"},
		{"directory", map[string]any{"path": "sub"}, "is a directory"},
		{"binary", map[string]any{"path": "bin.dat"}, "binary"},
		{"escape", map[string]any{"path": "../escape.txt"}, "outside the workspace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := run(t, r, "read_file", c.args)
			if !res.IsError {
				t.Fatalf("expected an error, got: %s", res.Content)
			}
			if !strings.Contains(res.Content, c.want) {
				t.Errorf("error = %q, want it to mention %q", res.Content, c.want)
			}
		})
	}
}

func TestWriteFileCreatesAndDiffs(t *testing.T) {
	r, root := newTestTools(t)

	res := run(t, r, "write_file", map[string]any{"path": "new/dir/f.txt", "content": "hello\n"})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	got, err := os.ReadFile(filepath.Join(root, "new", "dir", "f.txt"))
	if err != nil {
		t.Fatalf("file was not created: %v", err)
	}
	if string(got) != "hello\n" {
		t.Errorf("content = %q", got)
	}
	if !strings.Contains(res.Diff, "+hello") {
		t.Errorf("diff should record the addition, got %q", res.Diff)
	}
}

func TestWriteFileRefusesEscape(t *testing.T) {
	r, _ := newTestTools(t)
	res := run(t, r, "write_file", map[string]any{"path": "../../evil.txt", "content": "x"})
	if !res.IsError {
		t.Fatal("write outside the workspace must be refused")
	}
}

// Editing a file the agent has not read is how unseen work gets destroyed.
func TestEditRequiresPriorRead(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "a.txt", "alpha\nbeta\n")

	res := run(t, r, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "alpha", "new_string": "ALPHA",
	})
	if !res.IsError {
		t.Fatal("edit without a prior read must be refused")
	}
	if !strings.Contains(res.Content, "read") {
		t.Errorf("error should tell the model to read first, got %q", res.Content)
	}

	// After reading, the same edit must succeed.
	run(t, r, "read_file", map[string]any{"path": "a.txt"})
	res = run(t, r, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "alpha", "new_string": "ALPHA",
	})
	if res.IsError {
		t.Fatalf("edit after read failed: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "ALPHA\nbeta\n" {
		t.Errorf("content = %q", got)
	}
	if !strings.Contains(res.Diff, "-alpha") || !strings.Contains(res.Diff, "+ALPHA") {
		t.Errorf("diff = %q, want both sides of the change", res.Diff)
	}
}

func TestEditAmbiguityIsRefused(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "a.txt", "x\nx\nx\n")
	run(t, r, "read_file", map[string]any{"path": "a.txt"})

	res := run(t, r, "edit_file", map[string]any{"path": "a.txt", "old_string": "x", "new_string": "y"})
	if !res.IsError {
		t.Fatal("an ambiguous edit must be refused")
	}
	if !strings.Contains(res.Content, "3 times") {
		t.Errorf("error should say how many matches there were, got %q", res.Content)
	}

	// replace_all makes the intent explicit and must then be allowed.
	res = run(t, r, "edit_file", map[string]any{
		"path": "a.txt", "old_string": "x", "new_string": "y", "replace_all": true,
	})
	if res.IsError {
		t.Fatalf("replace_all failed: %s", res.Content)
	}
	got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
	if string(got) != "y\ny\ny\n" {
		t.Errorf("content = %q", got)
	}
}

func TestEditNoMatch(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "a.txt", "hello\n")
	run(t, r, "read_file", map[string]any{"path": "a.txt"})

	res := run(t, r, "edit_file", map[string]any{"path": "a.txt", "old_string": "absent", "new_string": "x"})
	if !res.IsError {
		t.Fatal("editing a string that is not present must fail")
	}
}

func TestListDir(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "a.txt", "x")
	write(t, root, "sub/b.txt", "y")

	res := run(t, r, "list_dir", map[string]any{})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "sub/") || !strings.Contains(res.Content, "a.txt") {
		t.Errorf("listing missing entries:\n%s", res.Content)
	}
}

func TestGlob(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "main.go", "package main")
	write(t, root, "src/app.go", "package src")
	write(t, root, "src/deep/x_test.go", "package deep")
	write(t, root, "notes.md", "hi")
	write(t, root, "node_modules/pkg/index.go", "ignored")

	cases := []struct {
		pattern string
		want    []string
		absent  []string
	}{
		{"**/*.go", []string{"main.go", "src/app.go", "src/deep/x_test.go"}, []string{"notes.md", "node_modules/pkg/index.go"}},
		{"*.go", []string{"main.go", "src/app.go"}, []string{"notes.md"}},
		{"src/**/*.go", []string{"src/app.go", "src/deep/x_test.go"}, []string{"main.go"}},
		{"**/*_test.go", []string{"src/deep/x_test.go"}, []string{"main.go"}},
	}
	for _, c := range cases {
		t.Run(c.pattern, func(t *testing.T) {
			res := run(t, r, "glob", map[string]any{"pattern": c.pattern})
			for _, w := range c.want {
				if !strings.Contains(res.Content, w) {
					t.Errorf("pattern %q missing %q:\n%s", c.pattern, w, res.Content)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(res.Content, a) {
					t.Errorf("pattern %q wrongly matched %q:\n%s", c.pattern, a, res.Content)
				}
			}
		})
	}
}

func TestGrep(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "a.go", "package main\nfunc main() {}\n")
	write(t, root, "b.go", "package main\nfunc helper() {}\n")
	write(t, root, "c.md", "func main is documented here\n")

	res := run(t, r, "grep", map[string]any{"pattern": `func main\(\)`})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go:2:") {
		t.Errorf("expected a.go line 2:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "b.go") {
		t.Errorf("b.go should not match:\n%s", res.Content)
	}

	// The glob filter must restrict which files are searched.
	res = run(t, r, "grep", map[string]any{"pattern": "func main", "glob": "**/*.md"})
	if !strings.Contains(res.Content, "c.md") || strings.Contains(res.Content, "a.go") {
		t.Errorf("glob filter not applied:\n%s", res.Content)
	}

	res = run(t, r, "grep", map[string]any{"pattern": "FUNC MAIN", "ignore_case": true})
	if !strings.Contains(res.Content, "a.go") {
		t.Errorf("ignore_case not applied:\n%s", res.Content)
	}

	res = run(t, r, "grep", map[string]any{"pattern": "nothing_matches_this"})
	if res.IsError || !strings.Contains(res.Content, "no matches") {
		t.Errorf("expected a clean no-match result, got %q", res.Content)
	}

	res = run(t, r, "grep", map[string]any{"pattern": "func ("})
	if !res.IsError {
		t.Error("an invalid regular expression must be reported as an error")
	}
}

func TestExec(t *testing.T) {
	r, _ := newTestTools(t)
	cmd := "echo hello"
	if runtime.GOOS == "windows" {
		cmd = "Write-Output hello"
	}
	res := run(t, r, "exec", map[string]any{"command": cmd})
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "hello") {
		t.Errorf("stdout not captured:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "[exit code 0]") {
		t.Errorf("exit code not reported:\n%s", res.Content)
	}
}

// A failing command is information for the model, not a tool fault: it must
// come back as a normal result carrying the exit code.
func TestExecNonZeroExitIsNotAToolError(t *testing.T) {
	r, _ := newTestTools(t)
	cmd := "exit 3"
	res := run(t, r, "exec", map[string]any{"command": cmd})
	if res.IsError {
		t.Errorf("a non-zero exit must not be a tool error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "[exit code 3]") {
		t.Errorf("exit code not reported:\n%s", res.Content)
	}
}

func TestExecTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sleep syntax differs on PowerShell")
	}
	r, _ := newTestTools(t)
	res := run(t, r, "exec", map[string]any{"command": "sleep 5", "timeout_seconds": 1})
	if !strings.Contains(res.Content, "timed out") {
		t.Errorf("expected a timeout notice:\n%s", res.Content)
	}
}

func TestExecRunsInWorkspace(t *testing.T) {
	r, root := newTestTools(t)
	write(t, root, "marker.txt", "x")
	cmd := "ls"
	if runtime.GOOS == "windows" {
		cmd = "Get-ChildItem -Name"
	}
	res := run(t, r, "exec", map[string]any{"command": cmd})
	if !strings.Contains(res.Content, "marker.txt") {
		t.Errorf("command did not run in the workspace:\n%s", res.Content)
	}
}

func TestExecCwdIsJailed(t *testing.T) {
	r, _ := newTestTools(t)
	res := run(t, r, "exec", map[string]any{"command": "echo x", "cwd": "../.."})
	if !res.IsError {
		t.Fatal("cwd outside the workspace must be refused")
	}
}

// Credentials in the operator's environment must not reach an approved
// command.
func TestSanitisedEnvDropsSecrets(t *testing.T) {
	t.Setenv("MY_API_TOKEN", "super-secret")
	t.Setenv("DB_PASSWORD", "hunter2")
	t.Setenv("MKR_API_KEY", "endpoint-key")
	t.Setenv("PATH_OK_VAR", "keep-me")

	env := sanitisedEnv()
	joined := strings.Join(env, "\n")
	for _, bad := range []string{"super-secret", "hunter2", "endpoint-key"} {
		if strings.Contains(joined, bad) {
			t.Errorf("sanitisedEnv leaked %q", bad)
		}
	}
	if !strings.Contains(joined, "keep-me") {
		t.Error("sanitisedEnv dropped an ordinary variable")
	}
}

func TestShellCommandArgv(t *testing.T) {
	cases := []struct {
		shell string
		want  []string
	}{
		{"pwsh", []string{"-NoProfile", "-NonInteractive", "-Command", "ls"}},
		{`C:\Program Files\PowerShell\7\pwsh.exe`, []string{"-NoProfile", "-NonInteractive", "-Command", "ls"}},
		{"powershell.exe", []string{"-NoProfile", "-NonInteractive", "-Command", "ls"}},
		{"cmd.exe", []string{"/C", "ls"}},
		{"/bin/bash", []string{"-c", "ls"}},
	}
	for _, c := range cases {
		_, args := shellCommand(c.shell, "ls")
		if strings.Join(args, "|") != strings.Join(c.want, "|") {
			t.Errorf("shellCommand(%q) args = %v, want %v", c.shell, args, c.want)
		}
	}
}
