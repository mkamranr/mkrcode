//go:build !windows

package ui

// enableVirtualTerminal is a no-op outside Windows, where terminals
// interpret escape sequences natively.
func enableVirtualTerminal() {}
