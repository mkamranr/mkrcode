// Package fsjail confines every filesystem path the model supplies to a
// single workspace directory.
//
// This is the security boundary of the program. A defect here is not a bug
// but a containment breach, so the package is deliberately small, has no
// dependencies beyond the standard library, and is exhaustively tested.
//
// Windows is the deployment target and its path grammar is considerably
// more hostile than POSIX: drive-relative paths, UNC and extended-length
// prefixes, reserved device names, alternate data streams, case
// insensitivity, and silent trimming of trailing dots and spaces are all
// routes out of a naive prefix check. Each is rejected explicitly below.
package fsjail

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrOutsideWorkspace is returned for any path that does not resolve
// beneath the jail root.
var ErrOutsideWorkspace = errors.New("path is outside the workspace")

// ErrUnsafePath is returned for paths whose form is rejected outright.
var ErrUnsafePath = errors.New("unsafe path")

// PathError describes a refused path without leaking the resolved location
// of anything outside the workspace.
type PathError struct {
	Path   string
	Reason string
	err    error
}

func (e *PathError) Error() string {
	return fmt.Sprintf("%s: %s", e.Path, e.Reason)
}

func (e *PathError) Unwrap() error { return e.err }

// Jail confines paths to Root.
type Jail struct {
	// root is absolute, cleaned, and symlink-resolved.
	root string
	// windows enables Win32 path-grammar rejections. It follows the host
	// platform by default and is forced on in tests so the Windows rules
	// are verifiable from any development machine.
	windows bool
}

// New returns a Jail rooted at dir, which must be an existing directory.
func New(dir string) (*Jail, error) {
	return newJail(dir, runtime.GOOS == "windows")
}

func newJail(dir string, windows bool) (*Jail, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("fsjail: a workspace root is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("fsjail: resolve root %q: %w", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("fsjail: workspace root %q: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("fsjail: workspace root %q is not a directory", abs)
	}
	// Resolving the root's own symlinks means later comparisons are
	// between two fully-resolved paths, never a mix.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return &Jail{root: filepath.Clean(abs), windows: windows}, nil
}

// Root returns the jail root.
func (j *Jail) Root() string { return j.root }

// Resolve validates p and returns the absolute path it denotes.
//
// Relative paths are interpreted against the workspace root. The returned
// path is safe to open: it is inside the workspace both lexically and after
// symlink resolution.
func (j *Jail) Resolve(p string) (string, error) {
	if err := j.checkForm(p); err != nil {
		return "", err
	}

	// Normalise separators so a Windows-style path supplied on any host is
	// interpreted the same way.
	q := p
	if j.windows {
		q = strings.ReplaceAll(q, "\\", "/")
	}

	abs := q
	if !isAbsolute(q, j.windows) {
		abs = filepath.Join(j.root, filepath.FromSlash(q))
	} else {
		abs = filepath.FromSlash(q)
	}
	abs = filepath.Clean(abs)

	// Lexical containment. This catches ".." traversal before any syscall.
	if !j.contains(abs) {
		return "", &PathError{Path: p, Reason: "resolves outside the workspace", err: ErrOutsideWorkspace}
	}

	// Symlink containment. A link inside the workspace may point outside
	// it, so the deepest existing ancestor is resolved and re-checked.
	// The path itself need not exist, because writes create new files.
	real, err := j.resolveExistingAncestor(abs)
	if err != nil {
		return "", &PathError{Path: p, Reason: err.Error(), err: ErrUnsafePath}
	}
	if !j.contains(real) {
		return "", &PathError{Path: p, Reason: "resolves outside the workspace through a symbolic link", err: ErrOutsideWorkspace}
	}
	return abs, nil
}

// Rel returns the workspace-relative form of an absolute path, for display.
func (j *Jail) Rel(abs string) string {
	rel, err := filepath.Rel(j.root, abs)
	if err != nil {
		return abs
	}
	return filepath.ToSlash(rel)
}

// contains reports whether abs is the root or lies beneath it.
func (j *Jail) contains(abs string) bool {
	root, path := j.root, filepath.Clean(abs)
	if j.windows {
		// Win32 filesystems are case-insensitive, so a case-varied prefix
		// would otherwise slip past the comparison.
		root, path = strings.ToLower(root), strings.ToLower(path)
	}
	if path == root {
		return true
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(root, sep) {
		root += sep
	}
	// The separator suffix is what stops "/wsX" matching root "/ws".
	return strings.HasPrefix(path, root)
}

// resolveExistingAncestor resolves symlinks on the longest existing prefix
// of abs and rejoins the non-existent remainder.
func (j *Jail) resolveExistingAncestor(abs string) (string, error) {
	remainder := ""
	cur := abs
	for i := 0; i < 256; i++ {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			if remainder == "" {
				return resolved, nil
			}
			return filepath.Join(resolved, remainder), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			// A permission error or a symlink loop must not be treated as
			// "does not exist"; refuse rather than guess.
			return "", fmt.Errorf("cannot resolve path: %v", err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the volume root without finding anything that exists.
			return abs, nil
		}
		remainder = filepath.Join(filepath.Base(cur), remainder)
		cur = parent
	}
	return "", errors.New("path nesting is too deep to resolve safely")
}

// checkForm rejects path shapes that are unsafe regardless of where they
// resolve to.
func (j *Jail) checkForm(p string) error {
	if strings.TrimSpace(p) == "" {
		return &PathError{Path: p, Reason: "path is empty", err: ErrUnsafePath}
	}
	if strings.ContainsRune(p, 0) {
		return &PathError{Path: p, Reason: "path contains a NUL byte", err: ErrUnsafePath}
	}
	if !j.windows {
		return nil
	}

	norm := strings.ReplaceAll(p, "\\", "/")

	// Extended-length and device namespace prefixes bypass Win32 path
	// normalisation entirely, including the checks below.
	if strings.HasPrefix(norm, "//?/") || strings.HasPrefix(norm, "//./") {
		return &PathError{Path: p, Reason: "extended-length and device paths are not permitted", err: ErrUnsafePath}
	}
	// UNC paths address other machines, which is outside any workspace.
	if strings.HasPrefix(norm, "//") {
		return &PathError{Path: p, Reason: "UNC network paths are not permitted", err: ErrUnsafePath}
	}
	// A drive-relative path such as C:foo resolves against that drive's
	// current directory, which the process does not control.
	if isDriveRelative(norm) {
		return &PathError{Path: p, Reason: "drive-relative paths are ambiguous; use a path relative to the workspace", err: ErrUnsafePath}
	}

	for _, seg := range strings.Split(norm, "/") {
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		// An alternate data stream hides content behind a second name on
		// the same file. Only a drive letter may legitimately contain ':'.
		if strings.Contains(seg, ":") {
			return &PathError{Path: p, Reason: "alternate data streams are not permitted", err: ErrUnsafePath}
		}
		// Win32 silently strips trailing dots and spaces, so "evil.txt."
		// and "evil.txt" name the same file while comparing differently.
		if seg != strings.TrimRight(seg, ". ") {
			return &PathError{Path: p, Reason: "path segments may not end with a dot or space", err: ErrUnsafePath}
		}
		if isReservedDeviceName(seg) {
			return &PathError{Path: p, Reason: fmt.Sprintf("%q is a reserved device name", seg), err: ErrUnsafePath}
		}
	}
	return nil
}

// isAbsolute reports whether p is absolute under the given path grammar.
// filepath.IsAbs cannot be used directly because it follows the host, not
// the target, platform.
func isAbsolute(p string, windows bool) bool {
	if !windows {
		return strings.HasPrefix(p, "/")
	}
	if strings.HasPrefix(p, "//") {
		return true
	}
	if len(p) >= 3 && isDriveLetter(p[0]) && p[1] == ':' && p[2] == '/' {
		return true
	}
	// A leading slash with no drive is rooted on the current drive; treat
	// it as absolute so it is checked against the root rather than joined.
	return strings.HasPrefix(p, "/")
}

// isDriveRelative matches "C:foo", which is not the same as "C:/foo".
func isDriveRelative(p string) bool {
	return len(p) >= 2 && isDriveLetter(p[0]) && p[1] == ':' &&
		(len(p) == 2 || p[2] != '/')
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// reservedDeviceNames are the MS-DOS device names that Win32 still resolves
// in every directory. Opening one reaches a device, not a file.
var reservedDeviceNames = map[string]bool{
	"con": true, "prn": true, "aux": true, "nul": true,
	"com1": true, "com2": true, "com3": true, "com4": true, "com5": true,
	"com6": true, "com7": true, "com8": true, "com9": true,
	"lpt1": true, "lpt2": true, "lpt3": true, "lpt4": true, "lpt5": true,
	"lpt6": true, "lpt7": true, "lpt8": true, "lpt9": true,
}

// isReservedDeviceName reports whether seg names a DOS device. The check
// ignores any extension, because CON.txt still opens the console.
func isReservedDeviceName(seg string) bool {
	base := strings.ToLower(seg)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return reservedDeviceNames[strings.TrimRight(base, " ")]
}
