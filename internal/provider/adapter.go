package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Adapter reconciles mkr's tool protocol with whatever the served model
// actually supports.
//
// Two implementations exist. NativeAdapter uses the OpenAI tools/tool_calls
// fields, which requires vLLM to have been started with
// --enable-auto-tool-choice and a matching --tool-call-parser. XMLAdapter
// carries schemas in the system prompt and recovers calls from the text
// stream, which works on any model regardless of server flags.
//
// The adapter is chosen at runtime by Probe, so a wrong or missing
// --tool-call-parser degrades to a working fallback instead of failing.
type Adapter interface {
	// Name identifies the adapter in logs, probes and the audit trail.
	Name() string

	// PrepareRequest attaches tool definitions to an outgoing request,
	// by whatever mechanism the adapter uses.
	PrepareRequest(req *ChatRequest, tools []ToolDef) error

	// NewFilter returns a stateful filter for one streamed completion.
	// Filters are single-use and must not be shared across requests.
	NewFilter() Filter
}

// Filter transforms the raw event stream into logical events. It exists so
// the XML adapter can withhold in-band tool-call markup from the rendered
// output while the native adapter passes events straight through.
type Filter interface {
	// Filter handles one raw event, calling emit zero or more times.
	Filter(ev Event, emit func(Event) error) error
}

// NativeAdapter sends tools in the request and trusts the server's
// tool-call parser to return structured tool_calls.
type NativeAdapter struct{}

// Name implements Adapter.
func (NativeAdapter) Name() string { return "native" }

// PrepareRequest implements Adapter by populating the tools field.
func (NativeAdapter) PrepareRequest(req *ChatRequest, tools []ToolDef) error {
	if len(tools) == 0 {
		return nil
	}
	req.Tools = tools
	req.ToolChoice = "auto"
	return nil
}

// NewFilter implements Adapter with a pass-through filter.
func (NativeAdapter) NewFilter() Filter { return passthroughFilter{} }

type passthroughFilter struct{}

func (passthroughFilter) Filter(ev Event, emit func(Event) error) error { return emit(ev) }

// ToolPromptHeader introduces the tool schemas in the system prompt when
// the XML adapter is active.
const toolPromptHeader = `
# Tool use

You can call tools by emitting a tool call block. Emit nothing else on the
same line as the tags. To call a tool, write exactly:

<tool_call>
{"name": "<tool name>", "arguments": {<arguments object>}}
</tool_call>

Rules:
- Emit one tool call block per tool you wish to invoke.
- "arguments" must be a JSON object matching the tool's schema exactly.
- Do not wrap tool call blocks in markdown code fences.
- After emitting tool calls, stop and wait for the results.

## Available tools
`

// buildToolPrompt renders the tool schemas as system-prompt text.
func buildToolPrompt(tools []ToolDef) string {
	if len(tools) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(toolPromptHeader)
	for _, t := range tools {
		b.WriteString("\n### ")
		b.WriteString(t.Function.Name)
		b.WriteString("\n")
		if t.Function.Description != "" {
			b.WriteString(t.Function.Description)
			b.WriteString("\n")
		}
		b.WriteString("Parameters (JSON Schema):\n```json\n")
		b.WriteString(compactJSON(t.Function.Parameters))
		b.WriteString("\n```\n")
	}
	return b.String()
}

// compactJSON renders schema on one line, falling back to the raw bytes if
// it is not valid JSON.
func compactJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return string(raw)
	}
	return out.String()
}

// injectSystemPrompt appends text to the leading system message, creating
// one if the transcript does not already start with it.
func injectSystemPrompt(req *ChatRequest, text string) {
	if text == "" {
		return
	}
	if len(req.Messages) > 0 && req.Messages[0].Role == RoleSystem {
		req.Messages[0].Content += "\n" + text
		return
	}
	req.Messages = append([]Message{{Role: RoleSystem, Content: text}}, req.Messages...)
}

// SelectAdapter returns the adapter matching name.
func SelectAdapter(name string) (Adapter, error) {
	switch strings.ToLower(name) {
	case "native":
		return NativeAdapter{}, nil
	case "xml":
		return &XMLAdapter{}, nil
	}
	return nil, fmt.Errorf("unknown adapter %q", name)
}
