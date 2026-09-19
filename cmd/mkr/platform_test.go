package main

import "runtime"

// isWindows reports whether the tests are running on Windows, where file
// mode bits carry no meaning.
func isWindows() bool { return runtime.GOOS == "windows" }
