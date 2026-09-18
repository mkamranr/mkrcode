package provider

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Tag literals recognised in the text stream.
const (
	tagOpen  = "<tool_call>"
	tagClose = "</tool_call>"
)

// XMLAdapter carries tool schemas in the system prompt and recovers tool
// calls from the model's text output.
//
// It is the fallback for any model or server configuration where native
// tool calling is unavailable or misconfigured. Because it never depends on
// vLLM's --tool-call-parser flag, it works even when that flag is wrong.
//
// Two payload shapes are accepted inside the tags:
//
//	Hermes/JSON   {"name": "read_file", "arguments": {"path": "a.go"}}
//	Qwen3-Coder   <function=read_file><parameter=path>a.go</parameter></function>
//
// Qwen3-Coder is fine-tuned to emit the second form, so accepting only the
// first would silently break the primary target model.
type XMLAdapter struct {
	// schemas maps tool name to parameter name to JSON Schema type, and is
	// used to coerce the untyped values of the Qwen3-Coder form.
	schemas map[string]map[string]string
}

// Name implements Adapter.
func (*XMLAdapter) Name() string { return "xml" }

// PrepareRequest implements Adapter by moving tool schemas into the system
// prompt and leaving the request's tools field empty.
func (a *XMLAdapter) PrepareRequest(req *ChatRequest, tools []ToolDef) error {
	req.Tools = nil
	req.ToolChoice = ""
	if len(tools) == 0 {
		return nil
	}
	a.schemas = make(map[string]map[string]string, len(tools))
	for _, t := range tools {
		a.schemas[t.Function.Name] = extractParamTypes(t.Function.Parameters)
	}
	injectSystemPrompt(req, buildToolPrompt(tools))
	return nil
}

// NewFilter implements Adapter.
func (a *XMLAdapter) NewFilter() Filter {
	return &xmlFilter{schemas: a.schemas}
}

// extractParamTypes pulls property name to type from a JSON Schema object.
func extractParamTypes(raw json.RawMessage) map[string]string {
	var schema struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	out := map[string]string{}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return out
	}
	for name, p := range schema.Properties {
		out[name] = p.Type
	}
	return out
}

// xmlFilter is the stateful scanner for one streamed completion.
type xmlFilter struct {
	schemas map[string]map[string]string

	buf     strings.Builder // text not yet classified as prose or markup
	inCall  bool            // true once an open tag has been seen
	calls   []ToolCall
	counter int
}

// Filter implements Filter.
func (f *xmlFilter) Filter(ev Event, emit func(Event) error) error {
	switch ev.Kind {
	case EventText:
		f.buf.WriteString(ev.Text)
		return f.drain(emit)

	case EventDone:
		// Flush whatever is left. An unterminated tool call means the model
		// was cut off mid-call; surface the fragment as text rather than
		// discarding it silently, so the failure is visible.
		if rest := f.buf.String(); rest != "" {
			f.buf.Reset()
			if err := emit(Event{Kind: EventText, Text: rest}); err != nil {
				return err
			}
		}
		for _, tc := range f.calls {
			if err := emit(Event{Kind: EventToolCall, ToolCall: tc}); err != nil {
				return err
			}
		}
		f.calls = nil
		return emit(ev)

	default:
		// Reasoning text and any natively-parsed tool calls pass through.
		return emit(ev)
	}
}

// drain consumes as much of the buffer as can be classified, emitting prose
// and recording completed tool calls. Text that might be the start of a tag
// is held back until more input arrives.
func (f *xmlFilter) drain(emit func(Event) error) error {
	for {
		s := f.buf.String()

		if f.inCall {
			end := strings.Index(s, tagClose)
			if end < 0 {
				return nil // still accumulating the call body
			}
			body := s[:end]
			f.consume(len(tagClose) + end)
			f.inCall = false
			if tc, ok := f.parseCall(body); ok {
				f.calls = append(f.calls, tc)
			} else if err := emit(Event{Kind: EventText, Text: tagOpen + body + tagClose}); err != nil {
				// Unparseable markup is shown rather than swallowed.
				return err
			}
			continue
		}

		start := strings.Index(s, tagOpen)
		if start >= 0 {
			if start > 0 {
				if err := emit(Event{Kind: EventText, Text: s[:start]}); err != nil {
					return err
				}
			}
			f.consume(start + len(tagOpen))
			f.inCall = true
			continue
		}

		// No tag present. Emit everything except a trailing fragment that
		// could still grow into an opening tag.
		hold := partialTagSuffix(s, tagOpen)
		if len(s) > hold {
			if err := emit(Event{Kind: EventText, Text: s[:len(s)-hold]}); err != nil {
				return err
			}
			f.consume(len(s) - hold)
		}
		return nil
	}
}

// consume drops the first n bytes of the buffer.
func (f *xmlFilter) consume(n int) {
	s := f.buf.String()
	f.buf.Reset()
	f.buf.WriteString(s[n:])
}

// partialTagSuffix returns the length of the longest suffix of s that is a
// proper prefix of tag. That suffix must be withheld, because the next
// chunk may complete it into a real tag.
func partialTagSuffix(s, tag string) int {
	max := len(tag) - 1
	if len(s) < max {
		max = len(s)
	}
	for n := max; n > 0; n-- {
		if strings.HasSuffix(s, tag[:n]) {
			return n
		}
	}
	return 0
}

// parseCall interprets a tool-call body in either accepted shape.
func (f *xmlFilter) parseCall(body string) (ToolCall, bool) {
	body = strings.TrimSpace(body)
	body = stripCodeFence(body)

	if tc, ok := f.parseJSONCall(body); ok {
		return tc, true
	}
	if tc, ok := f.parseQwenCall(body); ok {
		return tc, true
	}
	return ToolCall{}, false
}

// parseJSONCall handles {"name": ..., "arguments": {...}}.
func (f *xmlFilter) parseJSONCall(body string) (ToolCall, bool) {
	var payload struct {
		Name string `json:"name"`
		// Arguments may arrive as an object or, from some fine-tunes, as a
		// JSON-encoded string. RawMessage defers that decision.
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil || payload.Name == "" {
		return ToolCall{}, false
	}
	args := payload.Arguments
	if len(args) == 0 {
		args = payload.Parameters
	}
	return f.newCall(payload.Name, normaliseArgs(args)), true
}

// normaliseArgs renders arguments as a JSON object string, unwrapping the
// double-encoded case where arguments arrives as a JSON string.
func normaliseArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if json.Valid([]byte(asString)) {
			return asString
		}
	}
	return string(raw)
}

// parseQwenCall handles the Qwen3-Coder form:
//
//	<function=name>
//	<parameter=key>
//	value
//	</parameter>
//	</function>
func (f *xmlFilter) parseQwenCall(body string) (ToolCall, bool) {
	const fnOpen = "<function="
	i := strings.Index(body, fnOpen)
	if i < 0 {
		return ToolCall{}, false
	}
	rest := body[i+len(fnOpen):]
	gt := strings.Index(rest, ">")
	if gt < 0 {
		return ToolCall{}, false
	}
	name := strings.TrimSpace(strings.TrimSuffix(rest[:gt], "/"))
	if name == "" {
		return ToolCall{}, false
	}
	rest = rest[gt+1:]
	if end := strings.Index(rest, "</function>"); end >= 0 {
		rest = rest[:end]
	}

	types := f.schemas[name]
	args := map[string]any{}
	for {
		const pOpen = "<parameter="
		pi := strings.Index(rest, pOpen)
		if pi < 0 {
			break
		}
		rest = rest[pi+len(pOpen):]
		pgt := strings.Index(rest, ">")
		if pgt < 0 {
			break
		}
		key := strings.TrimSpace(rest[:pgt])
		rest = rest[pgt+1:]

		pend := strings.Index(rest, "</parameter>")
		var val string
		if pend < 0 {
			val, rest = rest, ""
		} else {
			val, rest = rest[:pend], rest[pend+len("</parameter>"):]
		}
		// The template puts the value on its own lines; those newlines are
		// framing, not content.
		args[key] = coerce(strings.Trim(val, "\r\n"), types[key])
		if rest == "" {
			break
		}
	}

	b, err := json.Marshal(args)
	if err != nil {
		return ToolCall{}, false
	}
	return f.newCall(name, string(b)), true
}

// coerce converts a textual parameter value to the type its schema declares.
// Unknown or unparseable types stay strings, which is the safe default: a
// tool receiving a string it expected to be a string is always correct.
func coerce(val, typ string) any {
	switch typ {
	case "integer":
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			return n
		}
	case "number":
		if n, err := strconv.ParseFloat(strings.TrimSpace(val), 64); err == nil {
			return n
		}
	case "boolean":
		if b, err := strconv.ParseBool(strings.TrimSpace(val)); err == nil {
			return b
		}
	case "array", "object":
		var v any
		if err := json.Unmarshal([]byte(strings.TrimSpace(val)), &v); err == nil {
			return v
		}
	}
	return val
}

// newCall assigns a stable synthetic ID, since the text protocol carries none.
func (f *xmlFilter) newCall(name, args string) ToolCall {
	f.counter++
	return ToolCall{
		ID:       fmt.Sprintf("xmlcall_%d", f.counter),
		Type:     "function",
		Function: FunctionCall{Name: name, Arguments: args},
	}
}

// stripCodeFence removes a markdown fence wrapping the payload. Models
// sometimes add one despite the prompt telling them not to.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[nl+1:]
	}
	return strings.TrimSuffix(strings.TrimRight(s, "\n "), "```")
}
