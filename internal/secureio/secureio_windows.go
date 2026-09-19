//go:build windows

package secureio

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

// ConvertStringSecurityDescriptorToSecurityDescriptorW is not exposed by Go's
// syscall package, so it is resolved from advapi32 directly. A lazy DLL keeps
// the binary free of any dependency outside the standard library, which is
// what lets it ship as a single file.
var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	procSDDL = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
)

// sddlRevision1 is the only revision the SDDL parser accepts.
const sddlRevision1 = 1

// buildSDDL returns a security descriptor string granting full control to the
// current user, LocalSystem and Administrators, and to nobody else.
//
// "D:P" makes the DACL protected, so it does not inherit permissive entries
// from the parent directory. Without P, a profile directory that happens to
// grant Users access would silently widen this file.
func buildSDDL() (string, error) {
	token, err := syscall.OpenCurrentProcessToken()
	if err != nil {
		return "", fmt.Errorf("open process token: %w", err)
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("read token user: %w", err)
	}
	sid, err := user.User.Sid.String()
	if err != nil {
		return "", fmt.Errorf("render user SID: %w", err)
	}
	// FA is full access. SY is LocalSystem and BA is the built-in
	// Administrators group; both are included because excluding them breaks
	// backup and administrative recovery without improving confidentiality,
	// as an administrator can take ownership regardless.
	return fmt.Sprintf("D:P(A;;FA;;;%s)(A;;FA;;;SY)(A;;FA;;;BA)", sid), nil
}

// securityAttributes builds SECURITY_ATTRIBUTES from the owner-only SDDL.
// The returned free function releases the descriptor.
func securityAttributes() (*syscall.SecurityAttributes, func(), error) {
	sddl, err := buildSDDL()
	if err != nil {
		return nil, func() {}, err
	}
	sddlPtr, err := syscall.UTF16PtrFromString(sddl)
	if err != nil {
		return nil, func() {}, err
	}

	var sd uintptr
	ret, _, callErr := procSDDL.Call(
		uintptr(unsafe.Pointer(sddlPtr)),
		uintptr(sddlRevision1),
		uintptr(unsafe.Pointer(&sd)),
		0,
	)
	if ret == 0 {
		return nil, func() {}, fmt.Errorf("convert security descriptor: %w", callErr)
	}

	sa := &syscall.SecurityAttributes{
		SecurityDescriptor: sd,
		InheritHandle:      0,
	}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, func() { syscall.LocalFree(syscall.Handle(sd)) }, nil
}

// openAppend creates or opens path with an explicit owner-only DACL.
//
// An existing file keeps the ACL it was created with; this sets the ACL only
// at creation, which matches how the Unix path behaves.
func openAppend(path string) (*os.File, error) {
	sa, free, err := securityAttributes()
	if err != nil {
		// Falling back to the ordinary path is better than refusing to run,
		// but the caller should know the file is less protected.
		return nil, fmt.Errorf("secureio: cannot build a restrictive ACL for %s: %w", path, err)
	}
	defer free()

	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}

	handle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ,
		sa,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return nil, fmt.Errorf("secureio: create %s: %w", path, err)
	}

	f := os.NewFile(uintptr(handle), path)
	// OPEN_ALWAYS positions at the start; appends must go to the end.
	if _, err := f.Seek(0, os.SEEK_END); err != nil {
		f.Close()
		return nil, fmt.Errorf("secureio: seek to end of %s: %w", path, err)
	}
	return f, nil
}

// mkdirAllPrivate creates dir and its parents with an owner-only DACL.
func mkdirAllPrivate(dir string) error {
	if dir == "" {
		return nil
	}
	if info, err := os.Stat(dir); err == nil {
		if info.IsDir() {
			return nil
		}
		return fmt.Errorf("secureio: %s exists and is not a directory", dir)
	}

	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirAllPrivate(parent); err != nil {
			return err
		}
	}

	sa, free, err := securityAttributes()
	if err != nil {
		return fmt.Errorf("secureio: cannot build a restrictive ACL for %s: %w", dir, err)
	}
	defer free()

	dirPtr, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return err
	}
	if err := syscall.CreateDirectory(dirPtr, sa); err != nil {
		if err == syscall.ERROR_ALREADY_EXISTS {
			return nil
		}
		return fmt.Errorf("secureio: create directory %s: %w", dir, err)
	}
	return nil
}
