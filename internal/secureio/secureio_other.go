//go:build !windows

package secureio

import "os"

// openAppend relies on the Unix permission bits, which behave as documented.
func openAppend(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
}

func mkdirAllPrivate(dir string) error {
	return os.MkdirAll(dir, 0o700)
}
