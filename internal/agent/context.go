package agent

import (
	"fmt"
	"strings"

	"mkrcode/internal/provider"
)

// Context budgeting keeps the transcript inside the model's window.
//
// Without it a working session simply grows until vLLM rejects the request,
// which surfaces to the operator as a raw server error in the middle of a
// task. Compaction turns that hard failure into a graceful, visible loss of
// the least useful history.

// defaultCharsPerToken seeds the estimator before any real measurement is
// available. Code and English both sit near this figure for the byte-pair
// vocabularies these models use; the value only has to be close, because it
// is corrected after the first response.
const defaultCharsPerToken = 3.6

// completionReserve is the share of the window held back for the model's
// reply and for the tool schemas the adapter injects.
const completionReserve = 0.20

// compactThreshold is the fraction of the usable budget at which compaction
// starts. Leaving headroom means compaction happens before a request fails,
// not after.
const compactThreshold = 0.75

// keepRecentToolResults is how many of the most recent tool results keep
// their full content. Older ones are the first thing elided: a file read
// twenty turns ago is the least valuable text in the window.
const keepRecentToolResults = 6

// minKeptExchanges is the number of trailing messages never dropped, so the
// model always retains the immediate task even under heavy pressure.
const minKeptExchanges = 4

// budget tracks how much context is available and how much is in use.
type budget struct {
	// maxModelLen is the model's context window, zero when unknown.
	maxModelLen int
	// charsPerToken is the running estimate, recalibrated from real usage.
	charsPerToken float64
	// lastObserved is the prompt_tokens the server reported for the last
	// request, which is ground truth rather than an estimate.
	lastObserved int
}

func newBudget(maxModelLen int) *budget {
	return &budget{maxModelLen: maxModelLen, charsPerToken: defaultCharsPerToken}
}

// observe recalibrates the estimator against a real measurement.
//
// vLLM reports prompt_tokens for every request, so the exact size of the
// transcript we just sent is known. Comparing it with the character count of
// that same transcript yields a ratio that is correct for this model and
// this codebase, which is more accurate than any general heuristic and costs
// nothing. This is why no tokenizer dependency is needed.
func (b *budget) observe(msgs []provider.Message, promptTokens int) {
	if promptTokens <= 0 {
		return
	}
	b.lastObserved = promptTokens
	chars := countChars(msgs)
	if chars <= 0 {
		return
	}
	ratio := float64(chars) / float64(promptTokens)
	// Ignore implausible ratios; a malformed usage field should not poison
	// the estimator.
	if ratio < 1.0 || ratio > 20.0 {
		return
	}
	// Smooth rather than replace, so one unusual turn does not swing the
	// estimate and cause needless compaction on the next.
	b.charsPerToken = 0.7*b.charsPerToken + 0.3*ratio
}

// estimate returns the approximate token count of msgs.
func (b *budget) estimate(msgs []provider.Message) int {
	if b.charsPerToken <= 0 {
		b.charsPerToken = defaultCharsPerToken
	}
	return int(float64(countChars(msgs)) / b.charsPerToken)
}

// usable returns the token budget available to the transcript, or zero when
// the window size is unknown and no budgeting can be done.
func (b *budget) usable() int {
	if b.maxModelLen <= 0 {
		return 0
	}
	return int(float64(b.maxModelLen) * (1 - completionReserve))
}

// needsCompaction reports whether msgs should be reduced before sending.
func (b *budget) needsCompaction(msgs []provider.Message) bool {
	u := b.usable()
	if u <= 0 {
		return false
	}
	return b.estimate(msgs) > int(float64(u)*compactThreshold)
}

// countChars sums the characters a transcript will occupy on the wire,
// including tool-call arguments, which are frequently the largest part.
func countChars(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		n += len(m.Content) + len(m.Role) + len(m.Name) + len(m.ToolCallID)
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Name) + len(tc.Function.Arguments) + len(tc.ID)
		}
		// Per-message framing overhead in the chat template.
		n += 8
	}
	return n
}

// compactionResult describes what compaction did, for the operator and the
// audit log.
type compactionResult struct {
	// Elided is the number of tool results whose content was replaced.
	Elided int
	// Dropped is the number of messages removed entirely.
	Dropped int
	// BeforeTokens and AfterTokens are estimates either side of the work.
	BeforeTokens int
	AfterTokens  int
}

// Changed reports whether anything was actually reduced.
func (r compactionResult) Changed() bool { return r.Elided > 0 || r.Dropped > 0 }

// String renders the result for display.
func (r compactionResult) String() string {
	var parts []string
	if r.Elided > 0 {
		parts = append(parts, fmt.Sprintf("elided %d older tool result(s)", r.Elided))
	}
	if r.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("dropped %d older message(s)", r.Dropped))
	}
	if len(parts) == 0 {
		return "nothing to compact"
	}
	return fmt.Sprintf("context compaction: %s (~%d to ~%d tokens)",
		strings.Join(parts, ", "), r.BeforeTokens, r.AfterTokens)
}

// compact reduces msgs to fit the budget, returning the new transcript.
//
// The ladder is applied in order of increasing loss: elide the bodies of old
// tool results first, then drop whole exchanges. The system prompt and the
// most recent messages are always preserved.
func (b *budget) compact(msgs []provider.Message) ([]provider.Message, compactionResult) {
	res := compactionResult{BeforeTokens: b.estimate(msgs)}
	if len(msgs) == 0 {
		return msgs, res
	}

	out := append([]provider.Message(nil), msgs...)
	target := int(float64(b.usable()) * compactThreshold)

	// Step 1: elide the content of older tool results.
	out, res.Elided = elideOldToolResults(out, keepRecentToolResults)
	if b.estimate(out) <= target {
		res.AfterTokens = b.estimate(out)
		return out, res
	}

	// Step 2: drop the oldest exchanges, preserving tool-call integrity.
	out, res.Dropped = dropOldest(out, minKeptExchanges, func(candidate []provider.Message) bool {
		return b.estimate(candidate) <= target
	})

	res.AfterTokens = b.estimate(out)
	return out, res
}

// elidedMarker renders the placeholder left in place of a tool result body.
func elidedMarker(name string) string {
	if name == "" {
		name = "tool"
	}
	return fmt.Sprintf("[earlier %s output elided to stay within the context window; "+
		"call the tool again if you still need it]", name)
}

// elideOldToolResults replaces the content of all but the most recent
// keepRecent tool results. The messages themselves stay, so every
// tool_call_id still has a matching response.
func elideOldToolResults(msgs []provider.Message, keepRecent int) ([]provider.Message, int) {
	// Locate tool results from the end, so "recent" is counted backwards.
	var idx []int
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleTool {
			idx = append(idx, i)
		}
	}
	if len(idx) <= keepRecent {
		return msgs, 0
	}

	elided := 0
	for _, i := range idx[keepRecent:] {
		marker := elidedMarker(msgs[i].Name)
		// Eliding something already short saves nothing and only makes the
		// transcript harder to read.
		if len(msgs[i].Content) <= len(marker) {
			continue
		}
		msgs[i].Content = marker
		elided++
	}
	return msgs, elided
}

// dropOldest removes messages from the front until fits reports success or
// only the protected tail remains.
//
// The critical constraint: an assistant message carrying tool_calls and the
// tool messages answering it must be removed together. Leaving a tool result
// whose originating call has been dropped produces a dangling tool_call_id,
// which vLLM rejects with a 400 — turning a context problem into a hard
// failure. Dropping therefore advances in whole groups.
func dropOldest(msgs []provider.Message, minKeep int, fits func([]provider.Message) bool) ([]provider.Message, int) {
	system := []provider.Message{}
	body := msgs
	if len(msgs) > 0 && msgs[0].Role == provider.RoleSystem {
		system = msgs[:1]
		body = msgs[1:]
	}

	dropped := 0
	for len(body) > minKeep {
		n := groupLen(body)
		if n <= 0 || len(body)-n < minKeep {
			break
		}
		body = body[n:]
		dropped += n

		candidate := append(append([]provider.Message(nil), system...), body...)
		if fits(candidate) {
			return candidate, dropped
		}
	}
	return append(append([]provider.Message(nil), system...), body...), dropped
}

// groupLen returns the number of leading messages that must be removed
// together: one message, plus every tool result answering its tool calls.
func groupLen(body []provider.Message) int {
	if len(body) == 0 {
		return 0
	}
	head := body[0]
	if len(head.ToolCalls) == 0 {
		return 1
	}
	// Collect the tool results belonging to this assistant turn.
	want := make(map[string]bool, len(head.ToolCalls))
	for _, tc := range head.ToolCalls {
		want[tc.ID] = true
	}
	n := 1
	for n < len(body) && body[n].Role == provider.RoleTool {
		n++
	}
	return n
}

// orphanedToolResults reports tool messages whose originating call is absent.
// It backs the invariant test and is cheap enough to assert in development.
func orphanedToolResults(msgs []provider.Message) []string {
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
	}
	var orphans []string
	for _, m := range msgs {
		if m.Role == provider.RoleTool && m.ToolCallID != "" && !seen[m.ToolCallID] {
			orphans = append(orphans, m.ToolCallID)
		}
	}
	return orphans
}
