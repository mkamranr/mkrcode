// Package provider speaks the OpenAI-compatible chat completions protocol
// exposed by vLLM, and adapts between that wire format and mkr's internal
// message and tool representations.
package provider

import "encoding/json"

// Role identifies the author of a message.
type Role string

// The four roles the chat protocol defines.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry in the conversation transcript.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`

	// ToolCalls is set on assistant messages that invoke tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool-role message back to its originating call.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Name carries the tool name on tool-role messages. Some templates use it.
	Name string `json:"name,omitempty"`
}

// ToolCall is a model's request to invoke a single tool.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall carries the tool name and its JSON-encoded arguments.
// Arguments stays a string because the protocol streams it in fragments
// and it is only valid JSON once fully assembled.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDef describes a tool to the model.
type ToolDef struct {
	Type     string      `json:"type"`
	Function FunctionDef `json:"function"`
}

// FunctionDef is the name, description and JSON Schema of one tool.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// NewToolDef builds a function-type ToolDef from a marshalled schema.
func NewToolDef(name, description string, schema json.RawMessage) ToolDef {
	return ToolDef{
		Type: "function",
		Function: FunctionDef{
			Name:        name,
			Description: description,
			Parameters:  schema,
		},
	}
}

// ChatRequest is the body posted to /v1/chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream"`
	Temperature float64   `json:"temperature"`
	TopP        float64   `json:"top_p"`
	MaxTokens   int       `json:"max_tokens,omitempty"`

	// Tools and ToolChoice are omitted entirely by the XML adapter, which
	// carries schemas in the system prompt instead.
	Tools      []ToolDef `json:"tools,omitempty"`
	ToolChoice string    `json:"tool_choice,omitempty"`

	// StreamOptions asks vLLM to emit a final usage chunk.
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

// StreamOptions controls extra data included in a streamed response.
type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// Usage reports token accounting for a completion.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// chatChunk is one server-sent event from a streaming completion.
type chatChunk struct {
	ID      string        `json:"id"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *Usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int        `json:"index"`
	Delta        chunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type chunkDelta struct {
	Role      Role            `json:"role"`
	Content   string          `json:"content"`
	ToolCalls []toolCallDelta `json:"tool_calls"`
	// ReasoningContent is emitted by reasoning models behind vLLM's
	// --reasoning-parser. It is surfaced separately from Content so it can
	// be rendered dimly and kept out of the durable transcript.
	ReasoningContent string `json:"reasoning_content"`
}

// toolCallDelta is a partial tool call. The protocol streams arguments in
// fragments keyed by Index, so deltas must be accumulated by index.
type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// modelsResponse is the body of GET /v1/models.
type modelsResponse struct {
	Data []ModelInfo `json:"data"`
}

// ModelInfo describes one served model.
type ModelInfo struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
	// MaxModelLen is a vLLM extension; it is absent on other servers.
	MaxModelLen int `json:"max_model_len"`
}

// EventKind distinguishes the items yielded by a streaming completion.
type EventKind int

// The kinds of event a stream can yield.
const (
	// EventText is a fragment of assistant-visible prose.
	EventText EventKind = iota
	// EventReasoning is a fragment of hidden chain-of-thought.
	EventReasoning
	// EventToolCall is a fully assembled tool call, yielded at stream end.
	EventToolCall
	// EventDone marks normal termination and carries usage.
	EventDone
)

// Event is one item produced by a streaming completion.
type Event struct {
	Kind EventKind
	// Text is set for EventText and EventReasoning.
	Text string
	// ToolCall is set for EventToolCall.
	ToolCall ToolCall
	// Usage and FinishReason are set for EventDone.
	Usage        Usage
	FinishReason string
}
