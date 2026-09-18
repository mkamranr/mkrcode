package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// Client talks to a single vLLM OpenAI-compatible endpoint.
//
// The http.Client is injected rather than constructed here so that the
// egress-restricted transport from internal/netguard is the only way the
// binary reaches the network. Client never builds a default transport.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// NewClient returns a Client posting to baseURL using hc. hc must not be nil:
// requiring it makes the egress restriction impossible to bypass by accident.
func NewClient(baseURL, apiKey string, hc *http.Client) (*Client, error) {
	if hc == nil {
		return nil, errors.New("provider: an http.Client is required (use netguard.NewClient)")
	}
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		return nil, errors.New("provider: baseURL is required")
	}
	return &Client{baseURL: base, apiKey: apiKey, http: hc}, nil
}

// newRequest builds an authenticated request against the endpoint.
func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	return req, nil
}

// Models lists the models the endpoint is serving.
func (c *Client) Models(ctx context.Context) ([]ModelInfo, error) {
	req, err := c.newRequest(ctx, http.MethodGet, "/v1/models", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /v1/models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var mr modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		return nil, fmt.Errorf("decode /v1/models response: %w", err)
	}
	if len(mr.Data) == 0 {
		return nil, errors.New("endpoint reports no served models")
	}
	return mr.Data, nil
}

// Stream posts a streaming chat completion and invokes yield for each event.
//
// Returning a non-nil error from yield aborts the stream and is returned to
// the caller, which is how Ctrl+C cancellation and turn limits take effect.
// Cancelling ctx also aborts the in-flight request.
func (c *Client) Stream(ctx context.Context, req ChatRequest, yield func(Event) error) error {
	req.Stream = true
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "/v1/chat/completions", req)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A cancelled context is the operator pressing Ctrl+C, not a fault.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("POST /v1/chat/completions: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return apiError(resp)
	}
	return consumeStream(ctx, resp.Body, yield)
}

// consumeStream decodes the event stream, accumulating streamed tool-call
// fragments and emitting them once the stream completes.
func consumeStream(ctx context.Context, body io.Reader, yield func(Event) error) error {
	var (
		sse    = newSSEReader(body)
		acc    = newToolCallAccumulator()
		usage  Usage
		finish string
	)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, err := sse.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}

		var chunk chatChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// A single malformed chunk should not destroy a long turn, but
			// it must be visible rather than silently dropped.
			return fmt.Errorf("decode stream chunk: %w (payload: %s)", err, truncate(string(payload), 256))
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finish = *ch.FinishReason
			}
			if ch.Delta.ReasoningContent != "" {
				if err := yield(Event{Kind: EventReasoning, Text: ch.Delta.ReasoningContent}); err != nil {
					return err
				}
			}
			if ch.Delta.Content != "" {
				if err := yield(Event{Kind: EventText, Text: ch.Delta.Content}); err != nil {
					return err
				}
			}
			acc.add(ch.Delta.ToolCalls)
		}
	}

	for _, tc := range acc.calls() {
		if err := yield(Event{Kind: EventToolCall, ToolCall: tc}); err != nil {
			return err
		}
	}
	return yield(Event{Kind: EventDone, Usage: usage, FinishReason: finish})
}

// toolCallAccumulator reassembles tool calls whose names and arguments
// arrive across many deltas, keyed by the protocol's per-call index.
type toolCallAccumulator struct {
	byIndex map[int]*ToolCall
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{byIndex: make(map[int]*ToolCall)}
}

func (a *toolCallAccumulator) add(deltas []toolCallDelta) {
	for _, d := range deltas {
		tc, ok := a.byIndex[d.Index]
		if !ok {
			tc = &ToolCall{Type: "function"}
			a.byIndex[d.Index] = tc
		}
		if d.ID != "" {
			tc.ID = d.ID
		}
		if d.Type != "" {
			tc.Type = d.Type
		}
		if d.Function.Name != "" {
			tc.Function.Name = d.Function.Name
		}
		// Arguments are concatenated; they are only valid JSON when whole.
		tc.Function.Arguments += d.Function.Arguments
	}
}

// calls returns the assembled tool calls in protocol index order.
func (a *toolCallAccumulator) calls() []ToolCall {
	idx := make([]int, 0, len(a.byIndex))
	for i := range a.byIndex {
		idx = append(idx, i)
	}
	sort.Ints(idx)

	out := make([]ToolCall, 0, len(idx))
	for _, i := range idx {
		tc := *a.byIndex[i]
		if tc.Function.Name == "" {
			continue // an index that never named a function is not a call
		}
		if tc.ID == "" {
			tc.ID = fmt.Sprintf("call_%d", i)
		}
		out = append(out, tc)
	}
	return out
}

// apiError converts a non-200 response into a diagnosable error, including
// the server's message body, which vLLM uses to explain bad requests.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	msg := strings.TrimSpace(string(b))

	// vLLM returns {"error": {"message": "..."}} or {"message": "..."}.
	var env struct {
		Error   struct{ Message string } `json:"error"`
		Message string                   `json:"message"`
	}
	if json.Unmarshal(b, &env) == nil {
		if env.Error.Message != "" {
			msg = env.Error.Message
		} else if env.Message != "" {
			msg = env.Message
		}
	}
	if msg == "" {
		msg = resp.Status
	}
	return &APIError{StatusCode: resp.StatusCode, Message: msg}
}

// APIError is a non-2xx response from the endpoint.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("vllm returned %d: %s", e.StatusCode, e.Message)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
