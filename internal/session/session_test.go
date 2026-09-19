package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mkrcode/internal/provider"
)

func TestCreateAppendLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Create(dir, "sess-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	want := []provider.Message{
		{Role: provider.RoleUser, Content: "add validation"},
		{
			Role:    provider.RoleAssistant,
			Content: "on it",
			ToolCalls: []provider.ToolCall{{
				ID: "c1", Type: "function",
				Function: provider.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
			}},
		},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "read_file", Content: "file contents"},
	}
	for _, m := range want {
		if err := s.Append(m, map[string]string{"mode": "approve"}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir, "sess-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("loaded %d messages, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Role != want[i].Role || got[i].Content != want[i].Content {
			t.Errorf("message %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Tool-call structure must survive the round trip, or a resumed session
	// sends a transcript the server rejects.
	if len(got[1].ToolCalls) != 1 || got[1].ToolCalls[0].ID != "c1" {
		t.Errorf("tool calls were not preserved: %+v", got[1].ToolCalls)
	}
	if got[2].ToolCallID != "c1" {
		t.Errorf("tool_call_id was not preserved: %q", got[2].ToolCallID)
	}
}

func TestAppendIsIncrementalAcrossReopen(t *testing.T) {
	dir := t.TempDir()

	s, _ := Create(dir, "sess-1")
	s.Append(provider.Message{Role: provider.RoleUser, Content: "first"}, nil)
	s.Close()

	// Resuming appends to the same transcript rather than replacing it.
	s, _ = Create(dir, "sess-1")
	s.Append(provider.Message{Role: provider.RoleUser, Content: "second"}, nil)
	s.Close()

	got, err := Load(dir, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2; reopening must not truncate", len(got))
	}
	if got[0].Content != "first" || got[1].Content != "second" {
		t.Errorf("order or content wrong: %q, %q", got[0].Content, got[1].Content)
	}
}

// A hard kill leaves a partially written final line. Resuming must recover
// everything before it rather than failing outright.
func TestLoadRecoversFromTruncatedFinalLine(t *testing.T) {
	dir := t.TempDir()
	s, _ := Create(dir, "sess-1")
	for _, c := range []string{"one", "two", "three"} {
		s.Append(provider.Message{Role: provider.RoleUser, Content: c}, nil)
	}
	s.Close()

	path := filepath.Join(dir, "sess-1.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Chop the file mid-way through the last record.
	truncated := string(raw[:len(raw)-20])
	if err := os.WriteFile(path, []byte(truncated), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Load(dir, "sess-1")
	if err != nil {
		t.Fatalf("Load must tolerate a truncated tail, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("recovered %d messages, want the 2 complete ones", len(got))
	}
	if got[0].Content != "one" || got[1].Content != "two" {
		t.Errorf("recovered the wrong messages: %q, %q", got[0].Content, got[1].Content)
	}
}

func TestLoadMissingSessionIsAClearError(t *testing.T) {
	_, err := Load(t.TempDir(), "nope")
	if err == nil {
		t.Fatal("expected an error for a missing session")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error should name the session, got %q", err)
	}
}

func TestListOrdersNewestFirst(t *testing.T) {
	dir := t.TempDir()
	for i, id := range []string{"a", "b", "c"} {
		s, err := Create(dir, id)
		if err != nil {
			t.Fatal(err)
		}
		s.Append(provider.Message{Role: provider.RoleUser, Content: id}, nil)
		s.Close()
		// Ensure distinct modification times.
		touchLater(t, filepath.Join(dir, id+".jsonl"), i+1)
	}

	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("listed %d sessions, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].Modified.Before(all[i].Modified) {
			t.Errorf("sessions are not newest-first: %s before %s", all[i-1].ID, all[i].ID)
		}
	}
}

func TestLatestReturnsTheNewest(t *testing.T) {
	dir := t.TempDir()

	if _, ok, err := Latest(dir); err != nil || ok {
		t.Errorf("an empty directory should report no session, got ok=%t err=%v", ok, err)
	}

	for i, id := range []string{"old", "new"} {
		s, _ := Create(dir, id)
		s.Append(provider.Message{Role: provider.RoleUser, Content: id}, nil)
		s.Close()
		touchLater(t, filepath.Join(dir, id+".jsonl"), i+1)
	}

	latest, ok, err := Latest(dir)
	if err != nil || !ok {
		t.Fatalf("Latest: ok=%t err=%v", ok, err)
	}
	if latest.ID != "new" {
		t.Errorf("Latest = %q, want new", latest.ID)
	}
}

func TestListIgnoresUnrelatedFiles(t *testing.T) {
	dir := t.TempDir()
	s, _ := Create(dir, "real")
	s.Close()
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	os.Mkdir(filepath.Join(dir, "subdir"), 0o755)

	all, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].ID != "real" {
		t.Errorf("List = %+v, want only the transcript", all)
	}
}

func TestListMissingDirectoryIsNotAnError(t *testing.T) {
	all, err := List(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("a missing session directory should not be an error: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("got %d sessions, want none", len(all))
	}
}

func TestNewIDIsUniqueAndSortable(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		id := NewID()
		if seen[id] {
			t.Fatalf("NewID produced a duplicate: %s", id)
		}
		seen[id] = true
		// The timestamp prefix is what makes listings sort chronologically.
		if len(id) < 15 || id[8] != '-' {
			t.Errorf("NewID = %q, want a sortable timestamp prefix", id)
		}
		if strings.ContainsAny(id, `/\:*?"<>|`) {
			t.Errorf("NewID = %q contains a character invalid in a Windows filename", id)
		}
	}
}

// A nil Session must be usable, so callers need no nil checks on a path
// where persistence is disabled.
func TestNilSessionIsSafe(t *testing.T) {
	var s *Session
	if err := s.Append(provider.Message{Role: provider.RoleUser}, nil); err != nil {
		t.Errorf("Append on a nil session: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("Close on a nil session: %v", err)
	}
}

// touchLater sets a file's modification time to a fixed base plus n minutes,
// so listing order is deterministic regardless of filesystem timestamp
// resolution.
func touchLater(t *testing.T, path string, n int) {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mod := base.Add(time.Duration(n) * time.Minute)
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}
