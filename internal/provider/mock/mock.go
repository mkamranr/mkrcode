// Package mock provides a scriptable stand-in for a vLLM OpenAI-compatible
// server.
//
// It exists because the target deployment has no GPUs yet and no network
// route to them. Every layer above the provider, the agent loop, the
// permission modes, redaction and the audit trail, is verified against this
// server rather than against real hardware.
package mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// ToolCall is a native tool call the scripted model should emit.
type ToolCall struct {
	ID   string
	Name string
	Args string // JSON-encoded arguments object
}

// Turn is one scripted assistant response.
type Turn struct {
	// Text is prose streamed as content deltas.
	Text string
	// Reasoning is streamed as reasoning_content deltas.
	Reasoning string
	// ToolCalls are streamed as native tool_call deltas. Leave empty when
	// exercising the XML adapter; put the markup in Text instead.
	ToolCalls []ToolCall
	// ChunkSize splits Text into deltas of at most this many bytes.
	// Zero means one delta for the whole string.
	ChunkSize int
	// FinishReason defaults to "stop", or "tool_calls" when ToolCalls is set.
	FinishReason string

	// Status, when non-zero, makes this turn fail with an HTTP error
	// instead of streaming, so retry and error paths can be exercised.
	Status int
	// ErrorMessage accompanies Status.
	ErrorMessage string
	// TruncateAfter cuts the stream off after this many events, simulating
	// a server or network dropping mid-response.
	TruncateAfter int
}

// Server is a mock vLLM endpoint.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	turns []Turn
	idx   int

	// requests records every decoded chat request, for assertions.
	requests []map[string]any

	// ModelID is reported by /v1/models.
	ModelID string
	// MaxModelLen is reported by /v1/models.
	MaxModelLen int
	// SupportsNativeTools controls whether a request carrying a tools field
	// is answered with a native tool call. Setting it false simulates a
	// server started without --enable-auto-tool-choice, which is exactly
	// the condition the capability probe must detect.
	SupportsNativeTools bool
	// ProbeToolName identifies the capability-probe handshake. Requests
	// carrying this tool are answered out of band and never consume a
	// scripted turn, so a script describes the conversation only.
	ProbeToolName string
	// ProbeTurn, when set, is the response to a probe handshake. It exists
	// so error paths in the probe can be exercised.
	ProbeTurn *Turn
}

// New starts a mock server with the given scripted turns.
func New(turns ...Turn) *Server {
	s := &Server{
		turns:               turns,
		ModelID:             "Qwen/Qwen3-Coder-30B-A3B",
		MaxModelLen:         262144,
		SupportsNativeTools: true,
		ProbeToolName:       "mkr_probe",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	s.Server = httptest.NewServer(mux)
	return s
}

// URL returns the base URL, without the /v1 suffix.
func (s *Server) URL() string { return s.Server.URL }

// Host returns the host:port for netguard configuration.
func (s *Server) Host() string { return strings.TrimPrefix(s.Server.URL, "http://") }

// SetTurns replaces the script and rewinds to the first turn.
func (s *Server) SetTurns(turns ...Turn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = turns
	s.idx = 0
	s.requests = nil
}

// Requests returns every chat request the server received.
func (s *Server) Requests() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, len(s.requests))
	copy(out, s.requests)
	return out
}

// RequestCount reports how many chat completions were requested.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id":            s.ModelID,
			"object":        "model",
			"owned_by":      "vllm",
			"max_model_len": s.MaxModelLen,
		}},
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":{"message":"bad request body"}}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, body)
	turn := s.nextTurnLocked(body)
	s.mu.Unlock()

	if turn.Status != 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(turn.Status)
		msg := turn.ErrorMessage
		if msg == "" {
			msg = "mock error"
		}
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": msg}})
		return
	}

	s.stream(w, turn)
}

// nextTurnLocked picks the response for this request.
//
// A capability-probe handshake is answered separately and does not advance
// the script, so scripted turns line up one-to-one with conversation turns.
func (s *Server) nextTurnLocked(body map[string]any) Turn {
	if s.isProbeRequest(body) {
		if s.ProbeTurn != nil {
			return *s.ProbeTurn
		}
		if s.SupportsNativeTools {
			return Turn{
				ToolCalls:    []ToolCall{{ID: "probe_1", Name: s.ProbeToolName, Args: `{"ok":true}`}},
				FinishReason: "tool_calls",
			}
		}
		// A server without auto tool choice answers in prose instead.
		return Turn{Text: "I would call " + s.ProbeToolName + "."}
	}

	if s.idx < len(s.turns) {
		t := s.turns[s.idx]
		s.idx++
		return t
	}
	return Turn{Text: "ok"}
}

// isProbeRequest reports whether the request carries the probe tool.
func (s *Server) isProbeRequest(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, _ := fn["name"].(string); name == s.ProbeToolName {
			return true
		}
	}
	return false
}

// stream writes the turn as a text/event-stream response.
func (s *Server) stream(w http.ResponseWriter, turn Turn) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	sent := 0
	send := func(payload map[string]any) bool {
		if turn.TruncateAfter > 0 && sent >= turn.TruncateAfter {
			return false
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
		sent++
		return true
	}

	base := func(delta map[string]any, finish any) map[string]any {
		return map[string]any{
			"id":    "chatcmpl-mock",
			"model": s.ModelID,
			"choices": []map[string]any{{
				"index":         0,
				"delta":         delta,
				"finish_reason": finish,
			}},
		}
	}

	if !send(base(map[string]any{"role": "assistant"}, nil)) {
		return
	}
	for _, chunk := range split(turn.Reasoning, turn.ChunkSize) {
		if !send(base(map[string]any{"reasoning_content": chunk}, nil)) {
			return
		}
	}
	for _, chunk := range split(turn.Text, turn.ChunkSize) {
		if !send(base(map[string]any{"content": chunk}, nil)) {
			return
		}
	}
	for i, tc := range turn.ToolCalls {
		// Stream the name and arguments separately, which is how vLLM
		// actually emits them and what the accumulator must handle.
		if !send(base(map[string]any{"tool_calls": []map[string]any{{
			"index": i, "id": tc.ID, "type": "function",
			"function": map[string]any{"name": tc.Name, "arguments": ""},
		}}}, nil)) {
			return
		}
		for _, frag := range split(tc.Args, 5) {
			if !send(base(map[string]any{"tool_calls": []map[string]any{{
				"index":    i,
				"function": map[string]any{"arguments": frag},
			}}}, nil)) {
				return
			}
		}
	}

	finish := turn.FinishReason
	if finish == "" {
		if len(turn.ToolCalls) > 0 {
			finish = "tool_calls"
		} else {
			finish = "stop"
		}
	}
	if !send(base(map[string]any{}, finish)) {
		return
	}
	if !send(map[string]any{
		"id": "chatcmpl-mock", "model": s.ModelID, "choices": []any{},
		"usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20, "total_tokens": 120},
	}) {
		return
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// split chops s into pieces of at most n bytes, respecting UTF-8 boundaries.
func split(s string, n int) []string {
	if s == "" {
		return nil
	}
	if n <= 0 || n >= len(s) {
		return []string{s}
	}
	var out []string
	runes := []rune(s)
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[i:end]))
	}
	return out
}
