package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mkrcode/internal/audit"
	"mkrcode/internal/config"
	"mkrcode/internal/provider"
	"mkrcode/internal/tools"
	"mkrcode/internal/ui"
)

// taskTool runs a nested agent with its own conversation, returning only its
// final answer to the parent.
//
// The value is context economy. "Find every place that validates user input"
// can take thirty tool calls and tens of thousands of tokens; the parent only
// needs the conclusion. Delegating keeps that work out of the main window.
//
// It lives in this package rather than internal/tools because it has to
// construct an Agent, and agent already imports tools. Implementing
// tools.Tool here avoids an import cycle.
type taskTool struct {
	// parent is the agent that owns this tool.
	parent *Agent
}

func (t *taskTool) Name() string { return "task" }

func (t *taskTool) Description() string {
	return "Delegate a self-contained piece of work to a sub-agent with its own " +
		"context, and receive back only its findings. Use this for open-ended " +
		"investigation — searching the codebase, tracing how something works, " +
		"reviewing many files — where the intermediate reading is not worth " +
		"keeping. Give it a complete, standalone instruction: it cannot see " +
		"this conversation."
}

// Mutating reports true. A sub-agent can use mutating tools, so the call
// itself is treated as mutating; each action it takes is then gated
// individually as well.
func (t *taskTool) Mutating() bool { return true }

func (t *taskTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "description": {"type": "string", "description": "A few words naming the task, for the operator."},
    "prompt": {"type": "string", "description": "The complete, self-contained instruction. The sub-agent cannot see this conversation."},
    "read_only": {"type": "boolean", "description": "Restrict the sub-agent to read-only tools. Use true for investigation. Default false."}
  },
  "required": ["description", "prompt"]
}`)
}

type taskArgs struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	ReadOnly    bool   `json:"read_only"`
}

func (t *taskTool) Describe(args json.RawMessage) string {
	var a taskArgs
	if err := decodeArgs(args, &a); err != nil {
		return "task " + string(args)
	}
	kind := "task"
	if a.ReadOnly {
		kind = "read-only task"
	}
	return fmt.Sprintf("delegate %s: %s", kind, a.Description)
}

// subagentMaxTurns bounds a delegated task independently of the parent, so
// one delegation cannot consume the whole session's budget.
const subagentMaxTurns = 30

func (t *taskTool) Run(ctx context.Context, args json.RawMessage) (tools.Result, error) {
	var a taskArgs
	if err := decodeArgs(args, &a); err != nil {
		return tools.Errorf("%v", err), nil
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return tools.Errorf("prompt is required and must be self-contained; the sub-agent cannot see this conversation"), nil
	}

	sub, err := t.parent.newSubagent(a)
	if err != nil {
		return tools.Errorf("%v", err), nil
	}

	t.parent.render.Info("  delegating: %s", a.Description)

	if err := sub.Run(ctx, a.Prompt); err != nil {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		return tools.Errorf("the delegated task failed: %v", err), nil
	}

	answer := strings.TrimSpace(sub.lastAssistantText())
	if answer == "" {
		answer = "The sub-agent produced no findings."
	}

	// The parent's accounting must include work done on its behalf, or
	// /cost under-reports the session.
	u := sub.Usage()
	t.parent.usage.PromptTokens += u.PromptTokens
	t.parent.usage.CompletionTokens += u.CompletionTokens
	t.parent.usage.TotalTokens += u.TotalTokens

	return tools.Result{
		Content: answer,
		Display: fmt.Sprintf("task complete: %s (%d tokens)", a.Description, u.TotalTokens),
	}, nil
}

// newSubagent builds a nested agent sharing the parent's security context.
func (a *Agent) newSubagent(args taskArgs) (*Agent, error) {
	if a.depth >= maxSubagentDepth {
		return nil, fmt.Errorf("sub-agents may not delegate further; do this work directly")
	}

	cfg := a.cfg
	cfg.MaxTurns = subagentMaxTurns

	// The sub-agent shares the parent's permission engine, redactor and
	// audit log deliberately. Sharing the engine means an operator still
	// approves the sub-agent's writes in approve mode, remembered approvals
	// carry across, and deny rules apply. Delegation must never become a
	// route to acting unsupervised.
	sub := &Agent{
		cfg:      cfg,
		client:   a.client,
		adapter:  a.adapter,
		perms:    a.perms,
		red:      a.red,
		log:      a.log,
		sess:     a.sess,
		render:   &subagentRenderer{inner: a.render},
		model:    a.model,
		budget:   newBudget(a.budget.maxModelLen),
		depth:    a.depth + 1,
		registry: a.subRegistry(args.ReadOnly),
	}

	for _, t := range sub.registry.All() {
		sub.toolDefs = append(sub.toolDefs, provider.NewToolDef(t.Name(), t.Description(), t.Schema()))
	}
	sub.messages = []provider.Message{{
		Role:    provider.RoleSystem,
		Content: subagentSystemPrompt(cfg, sub.registry, args.ReadOnly),
	}}

	a.log.Log(audit.Record{
		Event:   audit.EventSubagentStart,
		Mode:    string(a.perms.Mode()),
		Summary: args.Description,
		Message: fmt.Sprintf("depth %d, read_only %t", sub.depth, args.ReadOnly),
	})
	return sub, nil
}

// maxSubagentDepth is one: the top-level agent may delegate, and a
// sub-agent may not. The limit is enforced structurally as well, by not
// registering the task tool in a sub-agent's registry, so a miscounted
// depth cannot produce unbounded recursion.
const maxSubagentDepth = 1

// subRegistry builds the sub-agent's tools, omitting task so it cannot
// delegate, and omitting mutating tools entirely when read_only was asked
// for.
func (a *Agent) subRegistry(readOnly bool) *tools.Registry {
	sub := tools.NewRegistry()
	for _, t := range a.registry.All() {
		if t.Name() == "task" {
			continue
		}
		if readOnly && t.Mutating() {
			continue
		}
		sub.Add(t)
	}
	return sub
}

// subagentSystemPrompt instructs the nested agent, which has no view of the
// parent conversation.
func subagentSystemPrompt(cfg config.Config, reg *tools.Registry, readOnly bool) string {
	var b strings.Builder
	b.WriteString(SystemPrompt(cfg, reg))
	b.WriteString(`

# You are a sub-agent

You have been given one self-contained task by another agent. You cannot see
its conversation and you cannot ask it questions.

Work the task to a conclusion, then reply with your findings in a form that is
useful on its own: what you found, where (with file paths and line numbers),
and anything the caller needs to know. Your reply is the only thing that
reaches the caller, so do not refer to "the above" or assume shared context.

Be concise. Report facts you verified, and say plainly when something could
not be determined.
`)
	if readOnly {
		b.WriteString("\nYou have read-only tools. You cannot change files or run commands.\n")
	}
	return b.String()
}

// lastAssistantText returns the final prose the agent produced, which is
// what a delegated task reports back.
func (a *Agent) lastAssistantText() string {
	for i := len(a.messages) - 1; i >= 0; i-- {
		if a.messages[i].Role == provider.RoleAssistant && strings.TrimSpace(a.messages[i].Content) != "" {
			return a.messages[i].Content
		}
	}
	return ""
}

// subagentRenderer keeps a delegated task's output from flooding the
// terminal. The operator sees progress, not a second full transcript, but
// permission prompts must still reach them, so Confirm is not intercepted.
type subagentRenderer struct {
	inner ui.Renderer
}

func (r *subagentRenderer) Banner(...string)     {}
func (r *subagentRenderer) AssistantText(string) {}
func (r *subagentRenderer) Reasoning(string)     {}
func (r *subagentRenderer) EndAssistant()        {}
func (r *subagentRenderer) ToolStart(_, summary string) {
	r.inner.Info("    · %s", summary)
}
func (r *subagentRenderer) ToolResult(string, bool)       {}
func (r *subagentRenderer) Info(string, ...any)           {}
func (r *subagentRenderer) Warn(f string, a ...any)       { r.inner.Warn(f, a...) }
func (r *subagentRenderer) Errorf(f string, a ...any)     { r.inner.Errorf(f, a...) }
func (r *subagentRenderer) Prompt(string) (string, error) { return "", nil }

// decodeArgs unmarshals tool arguments, mirroring the helper in the tools
// package so this file does not need to reach into it.
func decodeArgs(args json.RawMessage, dst any) error {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, dst); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}
