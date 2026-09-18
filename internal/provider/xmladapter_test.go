package provider

import (
	"encoding/json"
	"strings"
	"testing"
)

// runFilter feeds text through an XML filter in the given chunk sizes and
// returns the prose emitted and the tool calls recovered.
func runFilter(t *testing.T, schemas map[string]map[string]string, text string, chunk int) (string, []ToolCall) {
	t.Helper()
	f := &xmlFilter{schemas: schemas}

	var prose strings.Builder
	var calls []ToolCall
	emit := func(ev Event) error {
		switch ev.Kind {
		case EventText:
			prose.WriteString(ev.Text)
		case EventToolCall:
			calls = append(calls, ev.ToolCall)
		}
		return nil
	}

	for i := 0; i < len(text); i += chunk {
		end := i + chunk
		if end > len(text) {
			end = len(text)
		}
		if err := f.Filter(Event{Kind: EventText, Text: text[i:end]}, emit); err != nil {
			t.Fatalf("Filter: %v", err)
		}
	}
	if err := f.Filter(Event{Kind: EventDone}, emit); err != nil {
		t.Fatalf("Filter done: %v", err)
	}
	return prose.String(), calls
}

func TestXMLExtractsJSONCall(t *testing.T) {
	text := `Let me read that file.
<tool_call>
{"name": "read_file", "arguments": {"path": "main.go"}}
</tool_call>`

	prose, calls := runFilter(t, nil, text, len(text))
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Errorf("name = %q, want read_file", calls[0].Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments are not valid JSON: %v", err)
	}
	if args["path"] != "main.go" {
		t.Errorf("path = %v, want main.go", args["path"])
	}
	if strings.Contains(prose, "tool_call") {
		t.Errorf("markup leaked into rendered prose: %q", prose)
	}
	if !strings.Contains(prose, "Let me read that file.") {
		t.Errorf("prose before the call was lost: %q", prose)
	}
}

// The critical property: identical results no matter how the server happens
// to chunk the stream. One byte at a time is the worst case, splitting every
// tag across many deltas.
func TestXMLSurvivesEveryChunkBoundary(t *testing.T) {
	text := `thinking<tool_call>
{"name": "grep", "arguments": {"pattern": "func main"}}
</tool_call>done`

	wantProse, wantCalls := runFilter(t, nil, text, len(text))
	if len(wantCalls) != 1 {
		t.Fatalf("baseline got %d calls, want 1", len(wantCalls))
	}

	for chunk := 1; chunk <= len(text); chunk++ {
		prose, calls := runFilter(t, nil, text, chunk)
		if len(calls) != 1 {
			t.Fatalf("chunk=%d: got %d calls, want 1", chunk, len(calls))
		}
		if calls[0].Function.Name != wantCalls[0].Function.Name {
			t.Errorf("chunk=%d: name = %q, want %q", chunk, calls[0].Function.Name, wantCalls[0].Function.Name)
		}
		if calls[0].Function.Arguments != wantCalls[0].Function.Arguments {
			t.Errorf("chunk=%d: arguments = %q, want %q", chunk, calls[0].Function.Arguments, wantCalls[0].Function.Arguments)
		}
		if prose != wantProse {
			t.Errorf("chunk=%d: prose = %q, want %q", chunk, prose, wantProse)
		}
		if strings.Contains(prose, "tool_call") {
			t.Errorf("chunk=%d: markup leaked: %q", chunk, prose)
		}
	}
}

// Qwen3-Coder is fine-tuned to emit this shape, so it must work even though
// it is not the format the prompt asks for.
func TestXMLExtractsQwenCoderForm(t *testing.T) {
	schemas := map[string]map[string]string{
		"read_file": {"path": "string", "limit": "integer", "verbose": "boolean"},
	}
	text := `<tool_call>
<function=read_file>
<parameter=path>
src/main.go
</parameter>
<parameter=limit>
120
</parameter>
<parameter=verbose>
true
</parameter>
</function>
</tool_call>`

	_, calls := runFilter(t, schemas, text, 7)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Function.Name != "read_file" {
		t.Fatalf("name = %q, want read_file", calls[0].Function.Name)
	}

	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if args["path"] != "src/main.go" {
		t.Errorf("path = %v, want src/main.go (framing newlines must be stripped)", args["path"])
	}
	// The schema says integer, so it must not arrive as the string "120".
	if n, ok := args["limit"].(float64); !ok || n != 120 {
		t.Errorf("limit = %#v, want the number 120 coerced from schema type", args["limit"])
	}
	if b, ok := args["verbose"].(bool); !ok || !b {
		t.Errorf("verbose = %#v, want the boolean true", args["verbose"])
	}
}

// Without a schema there is nothing to coerce against, so values stay
// strings. That is the safe default: never guess a type.
func TestXMLQwenFormWithoutSchemaKeepsStrings(t *testing.T) {
	text := "<tool_call><function=f><parameter=n>\n42\n</parameter></function></tool_call>"
	_, calls := runFilter(t, nil, text, 5)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("bad args: %v", err)
	}
	if args["n"] != "42" {
		t.Errorf("n = %#v, want the string \"42\"", args["n"])
	}
}

func TestXMLMultipleCalls(t *testing.T) {
	text := `<tool_call>{"name":"a","arguments":{}}</tool_call>mid<tool_call>{"name":"b","arguments":{}}</tool_call>`
	prose, calls := runFilter(t, nil, text, 3)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Function.Name != "a" || calls[1].Function.Name != "b" {
		t.Errorf("names = %q,%q want a,b", calls[0].Function.Name, calls[1].Function.Name)
	}
	if calls[0].ID == calls[1].ID {
		t.Errorf("call IDs must be distinct, both are %q", calls[0].ID)
	}
	if prose != "mid" {
		t.Errorf("prose = %q, want %q", prose, "mid")
	}
}

// Text that merely looks like the beginning of a tag must eventually be
// emitted, not held back forever.
func TestXMLFalseStartIsFlushed(t *testing.T) {
	text := "compare a <tool b and see"
	prose, calls := runFilter(t, nil, text, 1)
	if len(calls) != 0 {
		t.Fatalf("got %d calls, want 0", len(calls))
	}
	if prose != text {
		t.Errorf("prose = %q, want the full text %q", prose, text)
	}
}

// A stream cut off mid-call must surface the fragment rather than silently
// dropping the model's output.
func TestXMLUnterminatedCallIsSurfaced(t *testing.T) {
	text := `working<tool_call>{"name":"a","argum`
	prose, calls := runFilter(t, nil, text, 4)
	if len(calls) != 0 {
		t.Fatalf("got %d calls, want 0 for a truncated call", len(calls))
	}
	if !strings.Contains(prose, "working") {
		t.Errorf("prose lost the leading text: %q", prose)
	}
	if !strings.Contains(prose, "argum") {
		t.Errorf("truncated fragment was dropped instead of surfaced: %q", prose)
	}
}

// Unparseable markup must be shown to the operator, not swallowed.
func TestXMLGarbageCallIsSurfacedAsText(t *testing.T) {
	text := `<tool_call>this is not a call at all</tool_call>`
	prose, calls := runFilter(t, nil, text, 6)
	if len(calls) != 0 {
		t.Fatalf("got %d calls, want 0", len(calls))
	}
	if !strings.Contains(prose, "not a call") {
		t.Errorf("garbage body was swallowed: %q", prose)
	}
}

func TestXMLStripsCodeFence(t *testing.T) {
	text := "<tool_call>\n```json\n{\"name\":\"a\",\"arguments\":{\"k\":1}}\n```\n</tool_call>"
	_, calls := runFilter(t, nil, text, 9)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1 despite the markdown fence", len(calls))
	}
	if calls[0].Function.Name != "a" {
		t.Errorf("name = %q, want a", calls[0].Function.Name)
	}
}

// Some fine-tunes double-encode arguments as a JSON string.
func TestXMLUnwrapsDoubleEncodedArguments(t *testing.T) {
	text := `<tool_call>{"name":"a","arguments":"{\"path\":\"x.go\"}"}</tool_call>`
	_, calls := runFilter(t, nil, text, len(text))
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments were not unwrapped to an object: %v", err)
	}
	if args["path"] != "x.go" {
		t.Errorf("path = %v, want x.go", args["path"])
	}
}

func TestPartialTagSuffix(t *testing.T) {
	tests := []struct {
		s    string
		want int
	}{
		{"", 0},
		{"hello", 0},
		{"hello<", 1},
		{"hello<tool", 5},
		{"hello<tool_cal", 9},
		{"<tool_call", 10},
		{"a<b<too", 4},
	}
	for _, tt := range tests {
		if got := partialTagSuffix(tt.s, tagOpen); got != tt.want {
			t.Errorf("partialTagSuffix(%q) = %d, want %d", tt.s, got, tt.want)
		}
	}
}
