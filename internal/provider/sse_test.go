package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, raw string) []string {
	t.Helper()
	r := newSSEReader(strings.NewReader(raw))
	var out []string
	for {
		p, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, string(p))
	}
}

func TestSSEBasicFraming(t *testing.T) {
	got := readAll(t, "data: {\"a\":1}\n\ndata: {\"a\":2}\n\ndata: [DONE]\n\n")
	want := []string{`{"a":1}`, `{"a":2}`}
	if len(got) != len(want) {
		t.Fatalf("got %d events %q, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSSEIgnoresCommentsAndOtherFields(t *testing.T) {
	raw := ": heartbeat\nevent: message\nid: 7\ndata: {\"a\":1}\n\n: keepalive\n\ndata: [DONE]\n\n"
	got := readAll(t, raw)
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("got %q, want one event {\"a\":1}", got)
	}
}

func TestSSEHandlesCRLFAndNoSpaceAfterColon(t *testing.T) {
	got := readAll(t, "data:{\"a\":1}\r\n\r\ndata: [DONE]\r\n\r\n")
	if len(got) != 1 || got[0] != `{"a":1}` {
		t.Fatalf("got %q, want one event", got)
	}
}

// A multi-line data field is joined with newlines, per the SSE grammar.
func TestSSEJoinsMultiLineData(t *testing.T) {
	got := readAll(t, "data: line1\ndata: line2\n\ndata: [DONE]\n\n")
	if len(got) != 1 || got[0] != "line1\nline2" {
		t.Fatalf("got %q, want the two lines joined", got)
	}
}

// A body that ends without a trailing blank line must still yield its
// final event rather than dropping it. This is what a server disconnecting
// mid-stream looks like.
func TestSSEFlushesUnterminatedFinalEvent(t *testing.T) {
	got := readAll(t, "data: {\"a\":1}\n\ndata: {\"a\":2}")
	if len(got) != 2 {
		t.Fatalf("got %d events %q, want 2", len(got), got)
	}
	if got[1] != `{"a":2}` {
		t.Errorf("final event = %q, want the unterminated one", got[1])
	}
}

func TestSSEEmptyBody(t *testing.T) {
	if got := readAll(t, ""); len(got) != 0 {
		t.Fatalf("got %q, want no events", got)
	}
}
