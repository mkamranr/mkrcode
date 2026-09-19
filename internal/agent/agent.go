// Package agent runs the conversation loop: send the transcript to the
// model, stream the reply, execute any tools it calls, and repeat until the
// model stops calling tools.
//
// Every tool call passes through the same gate in the same order:
// permission, execution, redaction, audit. That ordering is the contract
// the rest of the system depends on, and it is enforced in one place here
// rather than in each tool.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"mkrcode/internal/audit"
	"mkrcode/internal/config"
	"mkrcode/internal/permission"
	"mkrcode/internal/provider"
	"mkrcode/internal/redact"
	"mkrcode/internal/session"
	"mkrcode/internal/tools"
	"mkrcode/internal/ui"
)

// Agent runs a session.
type Agent struct {
	cfg      config.Config
	client   *provider.Client
	adapter  provider.Adapter
	registry *tools.Registry
	perms    *permission.Engine
	red      *redact.Redactor
	log      *audit.Logger
	sess     *session.Session
	render   ui.Renderer

	// model is the served model ID resolved by the probe.
	model string
	// messages is the live transcript.
	messages []provider.Message
	// toolDefs is the schema list sent to the model each turn.
	toolDefs []provider.ToolDef
	// usage accumulates token counts across the session.
	usage provider.Usage
	// budget keeps the transcript inside the model's context window.
	budget *budget
}

// Options configures an Agent.
type Options struct {
	Config   config.Config
	Client   *provider.Client
	Adapter  provider.Adapter
	Registry *tools.Registry
	Perms    *permission.Engine
	Redactor *redact.Redactor
	Audit    *audit.Logger
	Session  *session.Session
	Renderer ui.Renderer
	Model    string
	// History seeds the transcript when resuming.
	History []provider.Message
	// SystemPrompt overrides the built-in prompt.
	SystemPrompt string
	// MaxModelLen is the context window reported by the probe. Zero
	// disables budgeting, which is the right behaviour for a server that
	// does not report one: guessing a window would be worse than not
	// compacting at all.
	MaxModelLen int
}

// New returns an Agent ready to run.
func New(opt Options) *Agent {
	a := &Agent{
		cfg:      opt.Config,
		client:   opt.Client,
		adapter:  opt.Adapter,
		registry: opt.Registry,
		perms:    opt.Perms,
		red:      opt.Redactor,
		log:      opt.Audit,
		sess:     opt.Session,
		render:   opt.Renderer,
		model:    opt.Model,
	}
	maxLen := opt.MaxModelLen
	if opt.Config.MaxModelLen > 0 {
		// An explicit configuration value overrides the probe, which is how
		// an operator constrains a session on a shared server.
		maxLen = opt.Config.MaxModelLen
	}
	a.budget = newBudget(maxLen)
	for _, t := range a.registry.All() {
		a.toolDefs = append(a.toolDefs, provider.NewToolDef(t.Name(), t.Description(), t.Schema()))
	}

	prompt := opt.SystemPrompt
	if prompt == "" {
		prompt = SystemPrompt(a.cfg, a.registry)
	}
	a.messages = append(a.messages, provider.Message{Role: provider.RoleSystem, Content: prompt})
	// Resumed history follows the system prompt, which is rebuilt each run
	// so a changed mode or workspace is reflected.
	for _, m := range opt.History {
		if m.Role == provider.RoleSystem {
			continue
		}
		a.messages = append(a.messages, m)
	}
	return a
}

// Usage returns cumulative token usage.
func (a *Agent) Usage() provider.Usage { return a.usage }

// Messages returns the live transcript.
func (a *Agent) Messages() []provider.Message { return a.messages }

// SetMode changes the permission mode mid-session and records the change.
func (a *Agent) SetMode(m config.Mode) {
	prev := a.perms.Mode()
	a.perms.SetMode(m)
	a.log.Log(audit.Record{
		Event:   audit.EventModeChange,
		Mode:    string(m),
		Message: fmt.Sprintf("mode changed from %s to %s", prev, m),
	})
}

// compactIfNeeded reduces the transcript when it approaches the context
// window, reporting what it did to the operator and the audit log.
func (a *Agent) compactIfNeeded() {
	if !a.budget.needsCompaction(a.messages) {
		return
	}
	a.applyCompaction("automatic")
}

// Compact reduces the transcript on request, backing the /compact command.
func (a *Agent) Compact() {
	if res := a.applyCompaction("manual"); !res.Changed() {
		a.render.Info("nothing to compact yet")
	}
}

// applyCompaction performs compaction and records it.
func (a *Agent) applyCompaction(trigger string) compactionResult {
	msgs, res := a.budget.compact(a.messages)
	if !res.Changed() {
		return res
	}
	a.messages = msgs

	a.render.Info("%s", res.String())
	a.log.Log(audit.Record{
		Event:   audit.EventContextCompaction,
		Mode:    string(a.perms.Mode()),
		Summary: res.String(),
		Message: trigger,
	})
	return res
}

// ContextUsage reports the estimated transcript size and the window, for the
// /cost command. A zero window means the server did not report one.
func (a *Agent) ContextUsage() (used, window int) {
	return a.budget.estimate(a.messages), a.budget.maxModelLen
}

// ErrMaxTurns is returned when a single request exceeds the turn budget.
var ErrMaxTurns = errors.New("reached the maximum number of tool-use turns")

// Run handles one user message, looping until the model stops calling
// tools or the turn budget is exhausted.
func (a *Agent) Run(ctx context.Context, userInput string) error {
	userMsg := provider.Message{Role: provider.RoleUser, Content: userInput}
	a.messages = append(a.messages, userMsg)
	a.sess.Append(userMsg, map[string]string{"mode": string(a.perms.Mode())})
	a.log.Log(audit.Record{
		Event:      audit.EventUserPrompt,
		Mode:       string(a.perms.Mode()),
		PromptHash: audit.HashPrompt(userInput),
		Prompt:     a.promptBody(userInput),
		Summary:    firstLine(userInput, 120),
	})

	for turn := 0; turn < a.cfg.MaxTurns; turn++ {
		calls, err := a.stream(ctx)
		if err != nil {
			return err
		}
		if len(calls) == 0 {
			return nil
		}

		stop, err := a.runTools(ctx, calls)
		if err != nil {
			return err
		}
		if stop {
			return nil
		}
	}

	a.render.Warn("stopped after %d tool-use turns; ask again to continue", a.cfg.MaxTurns)
	a.log.Log(audit.Record{Event: audit.EventError, Message: ErrMaxTurns.Error()})
	return nil
}

// stream sends the transcript and renders the reply, returning any tool
// calls the model made.
func (a *Agent) stream(ctx context.Context) ([]provider.ToolCall, error) {
	a.compactIfNeeded()

	req := provider.ChatRequest{
		Model:       a.model,
		Messages:    append([]provider.Message(nil), a.messages...),
		Temperature: a.cfg.Temperature,
		TopP:        a.cfg.TopP,
		MaxTokens:   a.cfg.MaxTokens,
	}
	if err := a.adapter.PrepareRequest(&req, a.toolDefs); err != nil {
		return nil, fmt.Errorf("prepare request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, a.cfg.RequestTimeout.D())
	defer cancel()

	var (
		text   strings.Builder
		calls  []provider.ToolCall
		usage  provider.Usage
		filter = a.adapter.NewFilter()
	)

	err := a.client.Stream(reqCtx, req, func(raw provider.Event) error {
		return filter.Filter(raw, func(ev provider.Event) error {
			switch ev.Kind {
			case provider.EventText:
				text.WriteString(ev.Text)
				a.render.AssistantText(ev.Text)
			case provider.EventReasoning:
				a.render.Reasoning(ev.Text)
			case provider.EventToolCall:
				calls = append(calls, ev.ToolCall)
			case provider.EventDone:
				usage = ev.Usage
			}
			return nil
		})
	})
	a.render.EndAssistant()

	if err != nil {
		// A cancelled context is the operator interrupting, not a fault.
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return nil, context.Canceled
		}
		a.log.Log(audit.Record{Event: audit.EventError, Message: err.Error(), Model: a.model})
		return nil, err
	}

	// The server's prompt_tokens is ground truth for the size of what we
	// just sent, so it recalibrates the estimator for the next turn.
	a.budget.observe(req.Messages, usage.PromptTokens)

	a.usage.PromptTokens += usage.PromptTokens
	a.usage.CompletionTokens += usage.CompletionTokens
	a.usage.TotalTokens += usage.TotalTokens

	msg := provider.Message{
		Role:      provider.RoleAssistant,
		Content:   text.String(),
		ToolCalls: calls,
	}
	a.messages = append(a.messages, msg)
	a.sess.Append(msg, nil)

	a.log.Log(audit.Record{
		Event:   audit.EventModelRequest,
		Model:   a.model,
		Adapter: a.adapter.Name(),
		Mode:    string(a.perms.Mode()),
		Tokens:  &audit.Tokens{Prompt: usage.PromptTokens, Completion: usage.CompletionTokens, Total: usage.TotalTokens},
		Summary: fmt.Sprintf("%d tool call(s)", len(calls)),
	})
	return calls, nil
}

// runTools executes each requested tool in order. It returns true when the
// session should stop.
func (a *Agent) runTools(ctx context.Context, calls []provider.ToolCall) (bool, error) {
	for _, call := range calls {
		if err := ctx.Err(); err != nil {
			return true, context.Canceled
		}

		tool, ok := a.registry.Get(call.Function.Name)
		if !ok {
			a.appendToolResult(call, fmt.Sprintf("unknown tool %q; available tools are: %s",
				call.Function.Name, strings.Join(a.registry.Names(), ", ")), true)
			continue
		}

		args := json.RawMessage(call.Function.Arguments)
		if len(args) == 0 || !json.Valid(args) {
			a.appendToolResult(call, fmt.Sprintf("arguments for %s were not valid JSON: %s",
				tool.Name(), truncate(call.Function.Arguments, 200)), true)
			continue
		}

		summary := tool.Describe(args)
		req := permission.Request{
			Tool:     tool.Name(),
			Mutating: tool.Mutating(),
			Summary:  summary,
			Paths:    writePaths(tool.Name(), args),
			Command:  commandOf(tool.Name(), args),
		}

		a.render.ToolStart(tool.Name(), summary)

		outcome, err := a.perms.Check(req)
		a.log.Log(audit.Record{
			Event:    audit.EventPermission,
			Tool:     tool.Name(),
			Summary:  summary,
			Args:     string(args),
			Mode:     string(a.perms.Mode()),
			Decision: outcome.Decision.String(),
			Reason:   outcome.Reason,
			Rule:     outcome.Rule,
			Paths:    req.Paths,
		})

		if err != nil {
			if errors.Is(err, ui.ErrQuit) {
				a.render.Info("session ended at a permission prompt")
				return true, nil
			}
			// A refusal is information the model can act on: it should
			// propose an alternative rather than the session dying.
			a.render.ToolResult(outcome.Reason, true)
			a.appendToolResult(call, "permission denied: "+outcome.Reason, true)
			continue
		}

		res, err := tool.Run(ctx, args)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return true, context.Canceled
			}
			a.render.ToolResult(err.Error(), true)
			a.appendToolResult(call, "tool failed: "+err.Error(), true)
			continue
		}

		// Redaction sits between execution and the transcript, so a secret
		// read off disk never reaches the model, the session file, or the
		// audit log.
		content, findings := a.red.Redact(res.Content)
		diff, diffFindings := a.red.Redact(res.Diff)
		findings = append(findings, diffFindings...)

		display := res.Display
		if display == "" {
			display = firstLine(content, 100)
		}
		if len(findings) > 0 {
			display += " (" + redact.Summary(findings) + ")"
			a.log.Log(audit.Record{
				Event:      audit.EventRedaction,
				Tool:       tool.Name(),
				Summary:    summary,
				Redactions: redact.Summary(findings),
			})
		}
		a.render.ToolResult(display, res.IsError)

		a.log.Log(audit.Record{
			Event:    audit.EventToolCall,
			Tool:     tool.Name(),
			Summary:  summary,
			Args:     string(args),
			Mode:     string(a.perms.Mode()),
			Decision: outcome.Decision.String(),
			Paths:    req.Paths,
			Diff:     diff,
			Message:  display,
		})

		a.appendToolResult(call, content, res.IsError)
	}
	return false, nil
}

// appendToolResult records a tool result in the transcript.
func (a *Agent) appendToolResult(call provider.ToolCall, content string, isError bool) {
	if isError && !strings.HasPrefix(content, "Error") {
		content = "Error: " + content
	}
	msg := provider.Message{
		Role:       provider.RoleTool,
		Content:    content,
		ToolCallID: call.ID,
		Name:       call.Function.Name,
	}
	a.messages = append(a.messages, msg)
	a.sess.Append(msg, nil)
}

// promptBody returns the prompt text for the audit log, honouring the
// audit_prompts setting. When off, only a hash is recorded.
func (a *Agent) promptBody(s string) string {
	if a.cfg.AuditPrompts {
		return s
	}
	return ""
}

// writePaths extracts the paths a call would modify, for the permission
// layer's protected-path rules.
func writePaths(tool string, args json.RawMessage) []string {
	switch tool {
	case "write_file", "edit_file":
		var a struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(args, &a) == nil && a.Path != "" {
			return []string{a.Path}
		}
	}
	return nil
}

// commandOf extracts the command line for the exec tool.
func commandOf(tool string, args json.RawMessage) string {
	if tool != "exec" {
		return ""
	}
	var a struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(args, &a) == nil {
		return a.Command
	}
	return ""
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, n)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Registry returns the tool set, for the /tools command.
func (a *Agent) Registry() *tools.Registry { return a.registry }

// Reset clears the conversation, keeping the system prompt. It backs the
// /clear command, which is how an operator recovers from a turn that has
// filled the context with unhelpful history.
func (a *Agent) Reset() {
	if len(a.messages) > 0 && a.messages[0].Role == provider.RoleSystem {
		a.messages = a.messages[:1]
		return
	}
	a.messages = nil
}
