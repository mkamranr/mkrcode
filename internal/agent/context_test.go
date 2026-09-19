package agent

import (
	"fmt"
	"strings"
	"testing"

	"mkrcode/internal/provider"
)

// buildTranscript makes a realistic transcript: a system prompt followed by
// n exchanges, each a user turn, an assistant turn calling a tool, and the
// tool's result carrying a large body.
func buildTranscript(n, bodyBytes int) []provider.Message {
	msgs := []provider.Message{{Role: provider.RoleSystem, Content: "system prompt"}}
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			provider.Message{Role: provider.RoleUser, Content: fmt.Sprintf("request %d", i)},
			provider.Message{
				Role:      provider.RoleAssistant,
				Content:   fmt.Sprintf("working on %d", i),
				ToolCalls: []provider.ToolCall{{ID: id, Type: "function", Function: provider.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}},
			},
			provider.Message{
				Role:       provider.RoleTool,
				ToolCallID: id,
				Name:       "read_file",
				Content:    strings.Repeat("x", bodyBytes),
			},
		)
	}
	return msgs
}

func TestBudgetDisabledWhenWindowUnknown(t *testing.T) {
	b := newBudget(0)
	msgs := buildTranscript(50, 10000)
	if b.needsCompaction(msgs) {
		t.Error("with no known window, compaction must not trigger; guessing a size is worse than not compacting")
	}
	if b.usable() != 0 {
		t.Errorf("usable = %d, want 0 when the window is unknown", b.usable())
	}
}

func TestBudgetTriggersOnLargeTranscript(t *testing.T) {
	b := newBudget(8192)
	if b.needsCompaction(buildTranscript(1, 50)) {
		t.Error("a small transcript must not trigger compaction")
	}
	if !b.needsCompaction(buildTranscript(40, 4000)) {
		t.Error("a transcript far over the window must trigger compaction")
	}
}

// The estimator must converge on the server's real figure, which is what
// removes the need for a tokenizer dependency.
func TestObserveRecalibratesEstimator(t *testing.T) {
	b := newBudget(32768)
	msgs := buildTranscript(4, 1000)
	chars := countChars(msgs)

	// Pretend the server reports a ratio of exactly 5 chars per token.
	realTokens := chars / 5
	for i := 0; i < 12; i++ {
		b.observe(msgs, realTokens)
	}

	got := b.estimate(msgs)
	drift := float64(got-realTokens) / float64(realTokens)
	if drift < -0.05 || drift > 0.05 {
		t.Errorf("estimate = %d, want within 5%% of the observed %d (ratio now %.2f)",
			got, realTokens, b.charsPerToken)
	}
}

func TestObserveIgnoresImplausibleReports(t *testing.T) {
	b := newBudget(32768)
	before := b.charsPerToken
	msgs := buildTranscript(2, 100)

	b.observe(msgs, 0)                    // no usage reported
	b.observe(msgs, countChars(msgs)*100) // absurdly low ratio
	b.observe(msgs, 1)                    // absurdly high ratio

	if b.charsPerToken != before {
		t.Errorf("charsPerToken = %.2f, want the seed %.2f; implausible reports must not poison the estimator",
			b.charsPerToken, before)
	}
}

// THE critical invariant. An assistant message carrying tool_calls and the
// tool messages answering it must survive or be removed together: a tool
// result whose originating call has been dropped is a dangling
// tool_call_id, which vLLM rejects with a 400. That would convert a context
// problem into a hard failure mid-task.
func TestCompactionNeverOrphansToolResults(t *testing.T) {
	for _, n := range []int{2, 5, 10, 25, 60} {
		for _, window := range []int{2048, 4096, 8192, 16384} {
			b := newBudget(window)
			msgs := buildTranscript(n, 2000)
			out, _ := b.compact(msgs)

			if orphans := orphanedToolResults(out); len(orphans) > 0 {
				t.Errorf("n=%d window=%d: compaction orphaned tool results %v", n, window, orphans)
			}
			// Every tool_call must also still be answered, or the model is
			// left waiting on a result that never arrives.
			answered := map[string]bool{}
			for _, m := range out {
				if m.Role == provider.RoleTool {
					answered[m.ToolCallID] = true
				}
			}
			for _, m := range out {
				for _, tc := range m.ToolCalls {
					if !answered[tc.ID] {
						t.Errorf("n=%d window=%d: tool call %s has no matching result", n, window, tc.ID)
					}
				}
			}
		}
	}
}

func TestCompactionPreservesSystemPrompt(t *testing.T) {
	b := newBudget(2048)
	msgs := buildTranscript(40, 3000)
	out, res := b.compact(msgs)

	if len(out) == 0 || out[0].Role != provider.RoleSystem {
		t.Fatal("the system prompt must always survive compaction")
	}
	if out[0].Content != "system prompt" {
		t.Errorf("the system prompt was altered: %q", out[0].Content)
	}
	if !res.Changed() {
		t.Error("an oversized transcript should have been reduced")
	}
}

func TestCompactionPreservesMostRecentMessages(t *testing.T) {
	b := newBudget(2048)
	msgs := buildTranscript(40, 3000)
	last := msgs[len(msgs)-1]

	out, _ := b.compact(msgs)
	if len(out) < 2 {
		t.Fatal("compaction removed too much")
	}
	got := out[len(out)-1]
	if got.ToolCallID != last.ToolCallID {
		t.Errorf("the most recent message was dropped; last id = %q, want %q", got.ToolCallID, last.ToolCallID)
	}
	// The most recent tool result must keep its real content, not a marker.
	if strings.Contains(got.Content, "elided") {
		t.Error("the most recent tool result must keep its content")
	}
}

// Eliding old tool bodies is the first and cheapest step, and should be
// enough on its own for a transcript that is only moderately over budget.
func TestCompactionEidesBeforeDropping(t *testing.T) {
	b := newBudget(16384)
	msgs := buildTranscript(20, 1500)
	out, res := b.compact(msgs)

	if res.Elided == 0 {
		t.Error("expected old tool results to be elided")
	}
	if res.Dropped > 0 {
		t.Errorf("dropped %d messages when eliding should have sufficed", res.Dropped)
	}
	// Eliding keeps the message count identical, which is what preserves
	// tool-call pairing for free.
	if len(out) != len(msgs) {
		t.Errorf("eliding changed the message count from %d to %d", len(msgs), len(out))
	}
}

func TestElideKeepsRecentResultsIntact(t *testing.T) {
	msgs := buildTranscript(20, 1000)
	out, elided := elideOldToolResults(msgs, keepRecentToolResults)
	if elided != 20-keepRecentToolResults {
		t.Errorf("elided %d, want %d", elided, 20-keepRecentToolResults)
	}

	intact := 0
	for _, m := range out {
		if m.Role == provider.RoleTool && !strings.Contains(m.Content, "elided") {
			intact++
		}
	}
	if intact != keepRecentToolResults {
		t.Errorf("%d tool results kept their content, want %d", intact, keepRecentToolResults)
	}
}

// Eliding must not replace short content with a longer marker, which would
// increase the size it is trying to reduce.
func TestElideSkipsAlreadyShortResults(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleSystem, Content: "s"},
	}
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("c%d", i)
		msgs = append(msgs,
			provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: id}}},
			provider.Message{Role: provider.RoleTool, ToolCallID: id, Name: "exec", Content: "ok"},
		)
	}
	before := countChars(msgs)
	out, _ := elideOldToolResults(msgs, 2)
	if after := countChars(out); after > before {
		t.Errorf("eliding grew the transcript from %d to %d characters", before, after)
	}
}

func TestCompactionActuallyReducesSize(t *testing.T) {
	b := newBudget(4096)
	msgs := buildTranscript(40, 3000)
	before := b.estimate(msgs)

	out, res := b.compact(msgs)
	after := b.estimate(out)

	if after >= before {
		t.Errorf("compaction did not reduce the transcript: %d -> %d", before, after)
	}
	if res.BeforeTokens != before || res.AfterTokens != after {
		t.Errorf("result reported %d -> %d, want %d -> %d", res.BeforeTokens, res.AfterTokens, before, after)
	}
	// It should get under the threshold it is aiming for.
	if target := int(float64(b.usable()) * compactThreshold); after > target {
		t.Errorf("after compaction %d tokens still exceeds the target %d", after, target)
	}
}

func TestCompactionOnSmallTranscriptIsANoOp(t *testing.T) {
	b := newBudget(32768)
	msgs := buildTranscript(2, 100)
	out, res := b.compact(msgs)
	if res.Changed() {
		t.Errorf("a small transcript was needlessly compacted: %s", res)
	}
	if len(out) != len(msgs) {
		t.Errorf("message count changed from %d to %d", len(msgs), len(out))
	}
}

func TestCompactionHandlesEmptyAndSystemOnly(t *testing.T) {
	b := newBudget(1024)
	if out, res := b.compact(nil); len(out) != 0 || res.Changed() {
		t.Error("compacting an empty transcript should do nothing")
	}
	only := []provider.Message{{Role: provider.RoleSystem, Content: strings.Repeat("x", 50000)}}
	out, _ := b.compact(only)
	if len(out) != 1 || out[0].Role != provider.RoleSystem {
		t.Error("a system-only transcript must be left intact even when oversized")
	}
}

// A transcript with no tool calls at all must still compact correctly.
func TestCompactionPlainConversation(t *testing.T) {
	b := newBudget(2048)
	msgs := []provider.Message{{Role: provider.RoleSystem, Content: "s"}}
	for i := 0; i < 40; i++ {
		msgs = append(msgs,
			provider.Message{Role: provider.RoleUser, Content: strings.Repeat("u", 500)},
			provider.Message{Role: provider.RoleAssistant, Content: strings.Repeat("a", 500)},
		)
	}
	out, res := b.compact(msgs)
	if !res.Changed() {
		t.Error("a long plain conversation should be compacted")
	}
	if out[0].Role != provider.RoleSystem {
		t.Error("the system prompt was lost")
	}
	if b.estimate(out) >= b.estimate(msgs) {
		t.Error("compaction did not reduce the transcript")
	}
}

func TestGroupLenKeepsToolCallsWithResults(t *testing.T) {
	body := []provider.Message{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: provider.RoleTool, ToolCallID: "a"},
		{Role: provider.RoleTool, ToolCallID: "b"},
		{Role: provider.RoleUser, Content: "next"},
	}
	if n := groupLen(body); n != 3 {
		t.Errorf("groupLen = %d, want 3 (the assistant turn plus both results)", n)
	}
	if n := groupLen(body[3:]); n != 1 {
		t.Errorf("groupLen on a plain message = %d, want 1", n)
	}
	if n := groupLen(nil); n != 0 {
		t.Errorf("groupLen(nil) = %d, want 0", n)
	}
}
