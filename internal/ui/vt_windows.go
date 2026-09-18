//go:build windows

package ui

import "syscall"

// enableVirtualTerminalProcessing makes the legacy Windows console
// interpret ANSI escape sequences. Windows Terminal enables it already;
// conhost.exe does not, and without this the session renders as a stream
// of raw escape codes.
const enableVirtualTerminalProcessing = 0x0004

// SetConsoleMode is absent from Go's syscall package, so it is resolved
// from kernel32 directly. Using a lazy DLL keeps the binary dependent on
// nothing outside the standard library, which is what allows it to ship as
// a single file.
var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// enableVirtualTerminal turns on VT processing for stdout. Any failure is
// ignored: the fallback is uncoloured output, not a broken session.
func enableVirtualTerminal() {
	handle, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return
	}
	var mode uint32
	if err := syscall.GetConsoleMode(handle, &mode); err != nil {
		return
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return
	}
	procSetConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
}
