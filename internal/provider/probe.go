package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Capabilities is what a Probe discovered about an endpoint.
type Capabilities struct {
	// Model is the served model ID reported by /v1/models.
	Model string
	// MaxModelLen is the context window, when the server reports one.
	MaxModelLen int
	// NativeTools is true when the server returned a well-formed tool call
	// in response to a request carrying a tools field.
	NativeTools bool
	// Adapter is the adapter selected on the basis of the above.
	Adapter string
	// Notes records anything the operator should know, such as the reason
	// a fallback was chosen.
	Notes []string
	// ProbedAt is when the probe ran.
	ProbedAt time.Time
}

// probeSchema is the parameter schema of the throwaway tool used to test
// whether the server can produce structured tool calls.
var probeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {"ok": {"type": "boolean", "description": "always true"}},
  "required": ["ok"]
}`)

// probeToolName is deliberately distinctive so a model echoing it in prose
// cannot be mistaken for a real call.
const probeToolName = "mkr_probe"

// probeTimeout bounds the handshake. A probe that hangs should not block
// startup indefinitely.
const probeTimeout = 60 * time.Second

// Probe determines how to talk to the endpoint.
//
// This is the mitigation for the project's central unknown: the served
// model and vLLM's --tool-call-parser flag are not known in advance, and at
// least one target model post-dates any documentation we can rely on. Rather
// than assuming native tool calling works, the probe asks the server to make
// one real tool call. If that succeeds the native adapter is used; if it
// does not, the XML adapter is selected, which works regardless of server
// configuration. A misconfigured endpoint therefore degrades instead of
// failing.
//
// When want is AdapterNative or AdapterXML the choice is honoured and only
// model metadata is collected.
func Probe(ctx context.Context, c *Client, want string, model string) (Capabilities, error) {
	caps := Capabilities{ProbedAt: time.Now(), Model: model}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	models, err := c.Models(ctx)
	if err != nil {
		return caps, fmt.Errorf("probe: cannot list models: %w", err)
	}
	if caps.Model == "" {
		caps.Model = models[0].ID
		if len(models) > 1 {
			caps.Notes = append(caps.Notes, fmt.Sprintf(
				"endpoint serves %d models and defaulted to %q; "+
					"choose one explicitly with: mkr config set model <id>",
				len(models), caps.Model))
		}
	}
	for _, m := range models {
		if m.ID == caps.Model && m.MaxModelLen > 0 {
			caps.MaxModelLen = m.MaxModelLen
		}
	}
	if caps.MaxModelLen == 0 {
		// max_model_len is a vLLM extension. Hosted OpenAI-compatible
		// providers do not report it, which leaves context budgeting with
		// nothing to budget against. Guessing a window would be worse than
		// not compacting, so say exactly how to supply it instead.
		caps.Notes = append(caps.Notes,
			"endpoint did not report a context window, so long sessions will not be compacted "+
				"automatically and may eventually be rejected by the server. "+
				"Set it with: mkr config set max_model_len <tokens>")
	}

	switch strings.ToLower(want) {
	case "native":
		caps.Adapter = "native"
		caps.NativeTools = true
		caps.Notes = append(caps.Notes, "adapter forced to native by configuration; not verified")
		return caps, nil
	case "xml":
		caps.Adapter = "xml"
		caps.Notes = append(caps.Notes, "adapter forced to xml by configuration")
		return caps, nil
	}

	native, why := testNativeTools(ctx, c, caps.Model)
	caps.NativeTools = native
	if native {
		caps.Adapter = "native"
	} else {
		caps.Adapter = "xml"
		caps.Notes = append(caps.Notes, "native tool calling unavailable ("+why+
			"); using the prompt-level xml adapter. To enable native tool calling, start vLLM with --enable-auto-tool-choice and a --tool-call-parser matching this model.")
	}
	return caps, nil
}

// testNativeTools issues one cheap completion carrying a tool definition and
// reports whether a usable tool call came back.
func testNativeTools(ctx context.Context, c *Client, model string) (bool, string) {
	req := ChatRequest{
		Model: model,
		Messages: []Message{
			{Role: RoleSystem, Content: "You are a capability probe. Call the " + probeToolName + " tool with ok=true. Do not reply with prose."},
			{Role: RoleUser, Content: "Call " + probeToolName + " now."},
		},
		Temperature: 0,
		MaxTokens:   64,
		Tools:       []ToolDef{NewToolDef(probeToolName, "Probe tool. Call it with ok=true.", probeSchema)},
		ToolChoice:  "auto",
	}

	var got *ToolCall
	err := c.Stream(ctx, req, func(ev Event) error {
		if ev.Kind == EventToolCall {
			tc := ev.ToolCall
			got = &tc
		}
		return nil
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			// vLLM rejects the tools field outright when auto tool choice
			// is disabled, which is a definitive negative answer.
			return false, fmt.Sprintf("server rejected the request with %d: %s", apiErr.StatusCode, truncate(apiErr.Message, 160))
		}
		return false, "probe request failed: " + truncate(err.Error(), 160)
	}
	if got == nil {
		return false, "the model returned no tool_calls field"
	}
	if got.Function.Name != probeToolName {
		return false, fmt.Sprintf("the model called %q instead of %q", got.Function.Name, probeToolName)
	}
	if !json.Valid([]byte(got.Function.Arguments)) {
		return false, "tool call arguments were not valid JSON"
	}
	return true, ""
}

// Summary renders the capabilities for the probe command's output.
func (c Capabilities) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model:        %s\n", c.Model)
	if c.MaxModelLen > 0 {
		fmt.Fprintf(&b, "context:      %d tokens\n", c.MaxModelLen)
	} else {
		fmt.Fprintf(&b, "context:      not reported\n")
	}
	fmt.Fprintf(&b, "native tools: %t\n", c.NativeTools)
	fmt.Fprintf(&b, "adapter:      %s\n", c.Adapter)
	for _, n := range c.Notes {
		fmt.Fprintf(&b, "note:         %s\n", n)
	}
	return b.String()
}
