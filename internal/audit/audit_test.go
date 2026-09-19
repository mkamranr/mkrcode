package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func newLogger(t *testing.T) (*Logger, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit", "mkr.jsonl")
	l, err := Open(Options{
		Path: path, Session: "sess-1", User: "operator", Host: "ws-01", Workspace: "/ws",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, path
}

func TestLogWritesChainedRecords(t *testing.T) {
	l, path := newLogger(t)

	for i := 0; i < 5; i++ {
		if err := l.Log(Record{Event: EventToolCall, Tool: "read_file", Summary: "read a.go"}); err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
	l.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 5 {
		t.Fatalf("got %d records, want 5", len(lines))
	}

	prev := genesisHash
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("record %d is not valid JSON: %v", i+1, err)
		}
		if r.Seq != int64(i+1) {
			t.Errorf("record %d has seq %d", i+1, r.Seq)
		}
		if r.PrevHash != prev {
			t.Errorf("record %d does not chain to its predecessor", i+1)
		}
		if r.Hash == "" {
			t.Errorf("record %d has no hash", i+1)
		}
		if r.Session != "sess-1" || r.User != "operator" || r.Host != "ws-01" {
			t.Errorf("record %d lost its identity fields", i+1)
		}
		prev = r.Hash
	}
}

func TestVerifyAcceptsAnIntactLog(t *testing.T) {
	l, path := newLogger(t)
	l.Log(Record{Event: EventSessionStart, Mode: "approve"})
	l.Log(Record{Event: EventToolCall, Tool: "write_file", Diff: "--- a\n+++ b\n+x\n"})
	l.Log(Record{Event: EventSessionEnd})
	l.Close()

	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res.OK {
		t.Fatalf("intact log failed verification: %s", res.Problem)
	}
	if res.Records != 3 {
		t.Errorf("Records = %d, want 3", res.Records)
	}
}

// The property that makes the log worth keeping: editing a record in the
// middle must be detected.
func TestVerifyDetectsModifiedRecord(t *testing.T) {
	l, path := newLogger(t)
	l.Log(Record{Event: EventToolCall, Tool: "exec", Summary: "go test"})
	l.Log(Record{Event: EventToolCall, Tool: "exec", Summary: "rm -rf important"})
	l.Log(Record{Event: EventToolCall, Tool: "exec", Summary: "go build"})
	l.Close()

	// Someone edits the incriminating record to look innocent.
	b, _ := os.ReadFile(path)
	tampered := strings.Replace(string(b), "rm -rf important", "ls -la harmless", 1)
	if tampered == string(b) {
		t.Fatal("test setup failed: nothing was replaced")
	}
	os.WriteFile(path, []byte(tampered), 0o600)

	res, err := Verify(path)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if res.OK {
		t.Fatal("verification passed on a tampered log")
	}
	if res.BrokenAt != 2 {
		t.Errorf("BrokenAt = %d, want 2", res.BrokenAt)
	}
	if !strings.Contains(res.Problem, "modified") {
		t.Errorf("problem = %q, want it to say the record was modified", res.Problem)
	}
}

// Deleting a record must break the chain too, otherwise inconvenient
// history could simply be removed.
func TestVerifyDetectsDeletedRecord(t *testing.T) {
	l, path := newLogger(t)
	for i := 0; i < 4; i++ {
		l.Log(Record{Event: EventToolCall, Tool: "exec", Summary: "cmd"})
	}
	l.Close()

	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	// Drop the second record.
	kept := append([]string{lines[0]}, lines[2:]...)
	os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600)

	res, _ := Verify(path)
	if res.OK {
		t.Fatal("verification passed after a record was deleted")
	}
	if res.BrokenAt != 2 {
		t.Errorf("BrokenAt = %d, want 2", res.BrokenAt)
	}
}

// Truncating the tail is the one edit a plain hash chain cannot detect on
// its own. The test documents that boundary honestly rather than asserting
// a property the design does not provide.
func TestVerifyAcceptsTruncatedTail(t *testing.T) {
	l, path := newLogger(t)
	for i := 0; i < 4; i++ {
		l.Log(Record{Event: EventToolCall, Tool: "exec"})
	}
	l.Close()

	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	os.WriteFile(path, []byte(strings.Join(lines[:2], "\n")+"\n"), 0o600)

	res, _ := Verify(path)
	if !res.OK {
		t.Fatal("a truncated log should still verify as a valid prefix")
	}
	if res.Records != 2 {
		t.Errorf("Records = %d, want 2", res.Records)
	}
	// The record count is what reveals truncation, which is why a
	// write-only collector is recommended alongside the local file.
}

// A second process must continue the chain, not start a competing one.
func TestReopenResumesTheChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	opt := Options{Path: path, Session: "s1", User: "u", Host: "h"}

	l1, err := Open(opt)
	if err != nil {
		t.Fatal(err)
	}
	l1.Log(Record{Event: EventSessionStart})
	l1.Log(Record{Event: EventSessionEnd})
	l1.Close()

	opt.Session = "s2"
	l2, err := Open(opt)
	if err != nil {
		t.Fatal(err)
	}
	l2.Log(Record{Event: EventSessionStart})
	l2.Close()

	res, err := Verify(path)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("the chain broke across sessions: %s", res.Problem)
	}
	if res.Records != 3 {
		t.Errorf("Records = %d, want 3", res.Records)
	}
}

func TestVerifyMissingFileIsNotAnError(t *testing.T) {
	res, err := Verify(filepath.Join(t.TempDir(), "absent.jsonl"))
	if err != nil {
		t.Fatalf("a missing log should not be an error: %v", err)
	}
	if !res.OK || res.Records != 0 {
		t.Errorf("got %+v, want an empty OK result", res)
	}
}

// The log is written after redaction, so it must never be the place a
// secret is preserved. This test pins the ordering contract.
func TestLogStoresOnlyWhatItIsGiven(t *testing.T) {
	l, path := newLogger(t)
	l.Log(Record{
		Event:      EventToolCall,
		Tool:       "read_file",
		Summary:    "read .env",
		Redactions: "redacted aws_access_key x1",
	})
	l.Close()

	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "AKIA") {
		t.Error("the audit log contains a secret")
	}
	if !strings.Contains(string(b), "redacted aws_access_key") {
		t.Error("the audit log should record that a redaction occurred")
	}
}

// The audit log must not be readable by other users of the machine.
//
// The two platforms express that differently. Unix permission bits are
// authoritative and are asserted directly. On Windows the mode bits are
// meaningless, so access is controlled by an explicit DACL applied at
// creation; that logic is verified in internal/secureio, and this test
// asserts only what is true on the running platform rather than a POSIX
// property Windows does not have.
func TestAuditFilePermissions(t *testing.T) {
	l, path := newLogger(t)
	l.Log(Record{Event: EventSessionStart})
	l.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// Confirm the file was created and is writable; confidentiality is
		// covered by the DACL tests in internal/secureio.
		if info.Size() == 0 {
			t.Error("the audit log is empty")
		}
		return
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("audit log mode is %o; it must not be group or world readable", perm)
	}
}

func TestHashPromptIsStableAndOpaque(t *testing.T) {
	a := HashPrompt("some prompt body")
	b := HashPrompt("some prompt body")
	c := HashPrompt("a different body")
	if a != b {
		t.Error("HashPrompt is not stable")
	}
	if a == c {
		t.Error("HashPrompt collided on different input")
	}
	if strings.Contains(a, "prompt") {
		t.Error("HashPrompt leaked its input")
	}
}
