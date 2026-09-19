package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"mkrcode/internal/netguard"
	"mkrcode/internal/provider/mock"
)

// newTestClient wires a Client to a mock server through the real
// egress-restricted transport, so tests exercise the production path.
func newTestClient(t *testing.T, s *mock.Server) *Client {
	t.Helper()
	hc, _, err := netguard.NewClient(s.Host(), 30*time.Second)
	if err != nil {
		t.Fatalf("netguard: %v", err)
	}
	c, err := NewClient(s.URL(), "", hc)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestNewClientRequiresHTTPClient(t *testing.T) {
	if _, err := NewClient("http://x:1", "", nil); err == nil {
		t.Fatal("NewClient must refuse a nil http.Client so egress cannot be bypassed")
	}
}

func TestProbeDetectsNativeToolSupport(t *testing.T) {
	s := mock.New()
	defer s.Close()
	s.SupportsNativeTools = true

	caps, err := Probe(context.Background(), newTestClient(t, s), "auto", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !caps.NativeTools {
		t.Error("NativeTools = false, want true against a server that supports them")
	}
	if caps.Adapter != "native" {
		t.Errorf("Adapter = %q, want native", caps.Adapter)
	}
	if caps.Model != "Qwen/Qwen3-Coder-30B-A3B" {
		t.Errorf("Model = %q, want the served model", caps.Model)
	}
	if caps.MaxModelLen != 262144 {
		t.Errorf("MaxModelLen = %d, want 262144", caps.MaxModelLen)
	}
}

// The central risk this project has to survive: vLLM started without
// --enable-auto-tool-choice, or with a --tool-call-parser that does not
// match the model. The probe must notice and fall back rather than fail.
func TestProbeFallsBackWhenNativeToolsUnavailable(t *testing.T) {
	s := mock.New()
	defer s.Close()
	s.SupportsNativeTools = false

	caps, err := Probe(context.Background(), newTestClient(t, s), "auto", "")
	if err != nil {
		t.Fatalf("Probe must not fail when native tools are missing: %v", err)
	}
	if caps.NativeTools {
		t.Error("NativeTools = true, want false")
	}
	if caps.Adapter != "xml" {
		t.Errorf("Adapter = %q, want the xml fallback", caps.Adapter)
	}
	joined := strings.Join(caps.Notes, " ")
	if !strings.Contains(joined, "--enable-auto-tool-choice") {
		t.Errorf("notes should tell the operator how to enable native tools, got %q", joined)
	}
}

// A server that rejects the tools field entirely is also a definitive
// negative, not a hard error.
func TestProbeFallsBackWhenServerRejectsTools(t *testing.T) {
	s := mock.New()
	defer s.Close()
	s.ProbeTurn = &mock.Turn{Status: 400, ErrorMessage: `"tools" is not supported: auto tool choice is disabled`}

	caps, err := Probe(context.Background(), newTestClient(t, s), "auto", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if caps.Adapter != "xml" {
		t.Errorf("Adapter = %q, want xml", caps.Adapter)
	}
	if !strings.Contains(strings.Join(caps.Notes, " "), "400") {
		t.Errorf("notes should carry the server's rejection, got %q", caps.Notes)
	}
}

func TestProbeHonoursForcedAdapter(t *testing.T) {
	s := mock.New()
	defer s.Close()
	s.SupportsNativeTools = true

	caps, err := Probe(context.Background(), newTestClient(t, s), "xml", "")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if caps.Adapter != "xml" {
		t.Errorf("Adapter = %q, want the forced xml", caps.Adapter)
	}
	// Forcing must not have issued a handshake completion.
	if s.RequestCount() != 0 {
		t.Errorf("forcing an adapter should skip the handshake, got %d requests", s.RequestCount())
	}
}

func TestProbeReportsUnreachableEndpoint(t *testing.T) {
	s := mock.New()
	c := newTestClient(t, s)
	s.Close() // the endpoint is now refusing connections

	if _, err := Probe(context.Background(), c, "auto", ""); err == nil {
		t.Fatal("Probe should fail when the endpoint is unreachable")
	}
}

func TestStreamAccumulatesToolCallArguments(t *testing.T) {
	s := mock.New(mock.Turn{
		Text: "on it. ",
		ToolCalls: []mock.ToolCall{
			{ID: "call_a", Name: "read_file", Args: `{"path":"main.go","limit":50}`},
		},
	})
	defer s.Close()

	var text strings.Builder
	var calls []ToolCall
	var usage Usage
	err := newTestClient(t, s).Stream(context.Background(), ChatRequest{Model: "m"}, func(ev Event) error {
		switch ev.Kind {
		case EventText:
			text.WriteString(ev.Text)
		case EventToolCall:
			calls = append(calls, ev.ToolCall)
		case EventDone:
			usage = ev.Usage
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text.String() != "on it. " {
		t.Errorf("text = %q", text.String())
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	// Arguments arrive in five-byte fragments and must reassemble exactly.
	if calls[0].Function.Arguments != `{"path":"main.go","limit":50}` {
		t.Errorf("arguments = %q, want the fragments reassembled", calls[0].Function.Arguments)
	}
	if usage.TotalTokens != 120 {
		t.Errorf("usage.TotalTokens = %d, want 120", usage.TotalTokens)
	}
}

// Ctrl+C must abort an in-flight stream promptly.
func TestStreamHonoursContextCancellation(t *testing.T) {
	s := mock.New(mock.Turn{Text: strings.Repeat("token ", 500), ChunkSize: 1})
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	err := newTestClient(t, s).Stream(ctx, ChatRequest{Model: "m"}, func(ev Event) error {
		if ev.Kind == EventText {
			n++
			if n == 3 {
				cancel()
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("Stream should return an error after cancellation")
	}
	cancel()
}

// An error returned by the consumer aborts the stream, which is how turn
// limits and interrupts take effect.
func TestStreamPropagatesConsumerError(t *testing.T) {
	s := mock.New(mock.Turn{Text: "aaaaaaaaaa", ChunkSize: 1})
	defer s.Close()

	sentinel := "stop now"
	err := newTestClient(t, s).Stream(context.Background(), ChatRequest{Model: "m"}, func(ev Event) error {
		if ev.Kind == EventText {
			return &stubErr{sentinel}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), sentinel) {
		t.Fatalf("err = %v, want the consumer's error", err)
	}
}

type stubErr struct{ s string }

func (e *stubErr) Error() string { return e.s }

func TestStreamSurfacesAPIError(t *testing.T) {
	s := mock.New(mock.Turn{Status: 503, ErrorMessage: "engine is still loading the model"})
	defer s.Close()

	err := newTestClient(t, s).Stream(context.Background(), ChatRequest{Model: "m"}, func(Event) error { return nil })
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "engine is still loading") {
		t.Errorf("err = %v, want the server's message preserved", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("err = %v, want the status code preserved", err)
	}
}

// Hosted OpenAI-compatible providers authenticate with a bearer token. This
// path is exercised nowhere else, and a silently missing header would look
// like an outage rather than a configuration mistake.
func TestAPIKeyIsSentAsBearerToken(t *testing.T) {
	s := mock.New(mock.Turn{Text: "hello"})
	defer s.Close()

	hc, _, err := netguard.NewClient(s.Host(), 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	const key = "sk-test-abcdef123456"
	c, err := NewClient(s.URL(), key, hc)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Models(context.Background()); err != nil {
		t.Fatalf("Models: %v", err)
	}
	if err := c.Stream(context.Background(), ChatRequest{Model: "m"}, func(Event) error { return nil }); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	headers := s.Headers()
	if len(headers) < 2 {
		t.Fatalf("recorded %d requests, want at least 2", len(headers))
	}
	for i, h := range headers {
		got := h.Get("Authorization")
		if got != "Bearer "+key {
			t.Errorf("request %d Authorization = %q, want the bearer token", i+1, got)
		}
	}
}

// With no key configured the header must be absent entirely, not sent empty:
// some servers reject "Bearer " with no value.
func TestNoAPIKeySendsNoAuthorizationHeader(t *testing.T) {
	s := mock.New(mock.Turn{Text: "hello"})
	defer s.Close()

	c := newTestClient(t, s)
	if _, err := c.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i, h := range s.Headers() {
		if _, ok := h["Authorization"]; ok {
			t.Errorf("request %d sent an Authorization header with no key configured", i+1)
		}
	}
}

// A provider that lists many models must not have one silently chosen for
// the operator without saying so.
func TestProbeReportsWhenManyModelsAreServed(t *testing.T) {
	s := mock.New()
	defer s.Close()
	s.ExtraModels = []string{"model-b", "model-c"}

	caps, err := Probe(context.Background(), newTestClient(t, s), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(caps.Notes, " ")
	if !strings.Contains(joined, "mkr config set model") {
		t.Errorf("notes should tell the operator how to choose: %q", joined)
	}
}

// Hosted providers do not report max_model_len, which silently disables
// context compaction. The operator must be told how to supply it.
func TestProbeReportsMissingContextWindow(t *testing.T) {
	s := mock.New()
	defer s.Close()
	s.MaxModelLen = 0

	caps, err := Probe(context.Background(), newTestClient(t, s), "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if caps.MaxModelLen != 0 {
		t.Fatalf("MaxModelLen = %d, want 0", caps.MaxModelLen)
	}
	joined := strings.Join(caps.Notes, " ")
	if !strings.Contains(joined, "mkr config set max_model_len") {
		t.Errorf("notes should give the exact command: %q", joined)
	}
}
