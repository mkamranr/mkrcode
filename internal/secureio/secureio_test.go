package secureio

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestOpenAppendCreatesAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")

	f, err := OpenAppend(path)
	if err != nil {
		t.Fatalf("OpenAppend: %v", err)
	}
	if _, err := f.WriteString("first\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	// Reopening must append, not truncate: an audit log that loses history
	// on the second session is worse than no audit log.
	f, err = OpenAppend(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := f.WriteString("second\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "first\nsecond\n" {
		t.Errorf("content = %q, want both lines in order", b)
	}
}

func TestOpenAppendOwnerOnlyOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses a DACL; see TestSDDLIsOwnerOnly")
	}
	path := filepath.Join(t.TempDir(), "log")
	f, err := OpenAppend(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("mode is %o; group and other must have no access", perm)
	}
}

func TestMkdirAllPrivateCreatesNestedDirs(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "a", "b", "c")

	if err := MkdirAllPrivate(dir); err != nil {
		t.Fatalf("MkdirAllPrivate: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("created path is not a directory")
	}

	// Idempotent: a second call on an existing directory must succeed.
	if err := MkdirAllPrivate(dir); err != nil {
		t.Errorf("second call failed: %v", err)
	}

	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("directory mode is %o; group and other must have no access", perm)
		}
	}
}

func TestMkdirAllPrivateRejectsAFile(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "notadir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllPrivate(file); err == nil {
		t.Error("creating a directory over an existing file must fail")
	}
}

func TestOpenAppendInMissingDirectoryFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "log")
	f, err := OpenAppend(path)
	if err == nil {
		f.Close()
		t.Error("opening a file in a nonexistent directory should fail; the caller must create it first")
	}
}
