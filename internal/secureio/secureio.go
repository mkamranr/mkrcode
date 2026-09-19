// Package secureio creates files and directories that only their owner can
// read.
//
// The audit log and session transcripts hold prompts, tool arguments, file
// diffs and source code. On a shared workstation those must not be readable
// by other users.
//
// This needs its own package because the Unix permission bits passed to
// os.OpenFile have almost no effect on Windows: the file is created with an
// inherited ACL and reports mode 0666 regardless of what was requested.
// Relying on the mode argument alone would leave the audit log readable by
// anyone on the machine, which a CI run on a real Windows host confirmed.
package secureio

import "os"

// OpenAppend opens path for appending, creating it with owner-only access.
func OpenAppend(path string) (*os.File, error) {
	return openAppend(path)
}

// MkdirAllPrivate creates dir and any parents, with owner-only access.
func MkdirAllPrivate(dir string) error {
	return mkdirAllPrivate(dir)
}
