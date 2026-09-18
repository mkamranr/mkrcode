package fsjail

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newWin builds a jail that applies Windows path rules regardless of the
// host, so the target platform's grammar is testable from any machine.
func newWin(t *testing.T, root string) *Jail {
	t.Helper()
	j, err := newJail(root, true)
	if err != nil {
		t.Fatalf("newJail: %v", err)
	}
	return j
}

func newPosix(t *testing.T, root string) *Jail {
	t.Helper()
	j, err := newJail(root, false)
	if err != nil {
		t.Fatalf("newJail: %v", err)
	}
	return j
}

func TestNewRejectsBadRoots(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("empty root should fail")
	}
	if _, err := New(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("nonexistent root should fail")
	}
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, []byte("x"), 0o600)
	if _, err := New(f); err == nil {
		t.Error("a file as root should fail")
	}
}

func TestResolveAcceptsPathsInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src", "pkg"), 0o755)
	j := newPosix(t, root)

	for _, p := range []string{
		"main.go",
		"./main.go",
		"src/pkg/x.go",
		"src/../main.go",
		"does/not/exist/yet.go", // writes create new files
		".",
	} {
		got, err := j.Resolve(p)
		if err != nil {
			t.Errorf("Resolve(%q) = error %v, want success", p, err)
			continue
		}
		if !strings.HasPrefix(got, j.Root()) {
			t.Errorf("Resolve(%q) = %q, which is outside the root %q", p, got, j.Root())
		}
	}
}

// The core containment property, expressed as the traversal attempts a
// model might actually produce.
func TestResolveRejectsTraversal(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ws")
	os.MkdirAll(root, 0o755)
	j := newPosix(t, root)

	for _, p := range []string{
		"..",
		"../",
		"../secret",
		"../../etc/passwd",
		"src/../../etc/passwd",
		"a/b/c/../../../../outside",
		"/etc/passwd",
		"/",
	} {
		if got, err := j.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) = %q, want refusal", p, got)
		} else if !errors.Is(err, ErrOutsideWorkspace) && !errors.Is(err, ErrUnsafePath) {
			t.Errorf("Resolve(%q) error = %v, want ErrOutsideWorkspace or ErrUnsafePath", p, err)
		}
	}
}

// A sibling directory sharing the root's name prefix must not be reachable.
// This is the classic off-by-one in prefix containment checks.
func TestResolveRejectsSiblingPrefixDirectory(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	sibling := filepath.Join(base, "ws-secrets")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(sibling, 0o755)
	os.WriteFile(filepath.Join(sibling, "key.pem"), []byte("secret"), 0o600)

	j := newPosix(t, root)
	if got, err := j.Resolve("../ws-secrets/key.pem"); err == nil {
		t.Errorf("Resolve reached the sibling directory: %q", got)
	}
	if j.contains(sibling) {
		t.Errorf("contains(%q) = true; %q must not be treated as inside %q", sibling, sibling, root)
	}
}

// A symlink inside the workspace pointing out of it is the subtlest escape,
// because the lexical check alone passes.
func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(root, 0o755)
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("classified"), 0o600)

	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	j := newPosix(t, root)

	if got, err := j.Resolve("link/secret.txt"); err == nil {
		t.Errorf("Resolve followed a symlink out of the workspace: %q", got)
	} else if !errors.Is(err, ErrOutsideWorkspace) {
		t.Errorf("error = %v, want ErrOutsideWorkspace", err)
	}
	// A symlink to a file directly, not just a directory.
	os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "flink"))
	if got, err := j.Resolve("flink"); err == nil {
		t.Errorf("Resolve followed a file symlink out of the workspace: %q", got)
	}
}

// A symlink that stays inside the workspace is legitimate and must work.
func TestResolveAllowsInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires elevation on Windows")
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "real"), 0o755)
	os.WriteFile(filepath.Join(root, "real", "a.txt"), []byte("x"), 0o600)
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	j := newPosix(t, root)
	if _, err := j.Resolve("alias/a.txt"); err != nil {
		t.Errorf("a symlink staying inside the workspace should resolve, got %v", err)
	}
}

// Every Windows-specific escape route, checked as one table.
func TestWindowsPathGrammarRejections(t *testing.T) {
	root := t.TempDir()
	j := newWin(t, root)

	tests := []struct {
		name string
		path string
	}{
		{"extended length prefix", `\\?\C:\Windows\System32\config\SAM`},
		{"device namespace", `\\.\PhysicalDrive0`},
		{"UNC share", `\\fileserver\share\secret.txt`},
		{"UNC forward slashes", "//fileserver/share/secret.txt"},
		{"drive relative", `C:notes.txt`},
		{"drive relative bare", `C:`},
		{"alternate data stream", `notes.txt:hidden`},
		{"ADS on nested file", `src/notes.txt:$DATA`},
		{"trailing dot", `evil.txt.`},
		{"trailing space", `evil.txt `},
		{"trailing dot nested", `src/evil.txt.`},
		{"device CON", `CON`},
		{"device CON lowercase", `con`},
		{"device with extension", `CON.txt`},
		{"device NUL nested", `src/nul`},
		{"device COM1", `COM1`},
		{"device LPT9", `lpt9.log`},
		{"absolute windows path", `C:\Windows\System32\drivers\etc\hosts`},
		{"backslash traversal", `..\..\Windows\win.ini`},
		{"mixed slash traversal", `src\..\..\outside.txt`},
		{"NUL byte", "a\x00b"},
		{"empty", ""},
		{"whitespace only", "   "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := j.Resolve(tt.path); err == nil {
				t.Errorf("Resolve(%q) = %q, want refusal", tt.path, got)
			}
		})
	}
}

// Legitimate Windows-style paths must still work; the grammar checks must
// not be so strict that ordinary use breaks.
func TestWindowsPathGrammarAcceptances(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "src"), 0o755)
	j := newWin(t, root)

	for _, p := range []string{
		`main.go`,
		`src\main.go`,
		`src/main.go`,
		`src\sub\new.txt`,
		`.\main.go`,
		`console.go`,    // contains "con" but is not the device
		`aux_helper.go`, // starts with "aux" but is not the device
		`com10.txt`,     // only com1-com9 are devices
		`nullable.go`,
	} {
		if _, err := j.Resolve(p); err != nil {
			t.Errorf("Resolve(%q) = %v, want success", p, err)
		}
	}
}

// Win32 filesystems are case-insensitive, so containment must be too;
// otherwise a case-varied root prefix escapes the check.
func TestWindowsContainmentIsCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	j := newWin(t, root)

	upper := strings.ToUpper(j.Root())
	if !j.contains(upper) {
		t.Errorf("contains(%q) = false; Windows containment must ignore case", upper)
	}
	// Case-insensitivity must not accidentally admit a sibling.
	if j.contains(j.Root() + "-other") {
		t.Error("a sibling with a shared prefix must not be contained")
	}
}

// POSIX filesystems are case-sensitive, so the same check must not ignore
// case there; doing so would wrongly admit a differently-cased sibling.
func TestPosixContainmentIsCaseSensitive(t *testing.T) {
	root := t.TempDir()
	j := newPosix(t, root)
	if j.contains(strings.ToUpper(j.Root()) + "/x") {
		t.Error("POSIX containment must respect case")
	}
}

func TestRelIsWorkspaceRelative(t *testing.T) {
	root := t.TempDir()
	j := newPosix(t, root)
	abs, err := j.Resolve("src/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Rel(abs); got != "src/main.go" {
		t.Errorf("Rel = %q, want src/main.go", got)
	}
}

// The root itself must resolve, since tools list and search it.
func TestResolveRootItself(t *testing.T) {
	root := t.TempDir()
	j := newPosix(t, root)
	got, err := j.Resolve(".")
	if err != nil {
		t.Fatalf("Resolve(\".\") = %v", err)
	}
	if got != j.Root() {
		t.Errorf("Resolve(\".\") = %q, want the root %q", got, j.Root())
	}
}

// Deeply nested nonexistent paths must not hang or blow the ancestor walk.
func TestResolveDeepNonexistentPath(t *testing.T) {
	root := t.TempDir()
	j := newPosix(t, root)
	deep := strings.Repeat("a/", 200) + "file.txt"
	if _, err := j.Resolve(deep); err != nil {
		t.Errorf("a deep nonexistent path should resolve, got %v", err)
	}
}
